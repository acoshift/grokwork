package bot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/agentapi"
	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/agentmcp"
	"github.com/acoshift/grokwork/internal/clickup"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
	"github.com/acoshift/grokwork/internal/errsrc/sentry"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/projstore"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

// agentPlane holds the in-session agent control plane (auth + API + UDS bridge).
type agentPlane struct {
	Auth    *agentauth.Store
	API     *agentapi.Service
	Storage *projstore.Store
	Bridge  *agentmcp.Bridge
	Sock    string
	started bool
}

func (b *Bot) initAgentPlane() {
	if b == nil || b.cfg == nil || b.cfg.DataDir == "" {
		return
	}
	storage, err := projstore.New(b.cfg.DataDir)
	if err != nil {
		log.Printf("warn: projstore: %v", err)
		return
	}
	auth := agentauth.NewStore()
	api := &agentapi.Service{
		Auth:     auth,
		Sessions: b.sessions,
		Reviews:  b.reviews,
		Storage:  storage,
		Bot:      b,
		Audit:    b.audit,
		EligibleReviewer: func(project, reviewerID string) bool {
			return b.eligibleTeamReviewer(project, reviewerID)
		},
		DisplayName: func(id string) string {
			if id == "" {
				return ""
			}
			return id
		},
		RepoDir: func(project string) string {
			if b.cfg == nil {
				return ""
			}
			path, ok := b.cfg.ProjectPath(project)
			if !ok {
				return ""
			}
			return path
		},
		GH:                   b.ghRunner,
		ClickUpEnabled:       b.cfg.ProjectClickUpEnabled,
		ClickUpAPIKey:        b.cfg.ProjectClickUpAPIKey,
		ClickUpWorkspaceID:   b.cfg.ProjectClickUpWorkspaceID,
		ClickUpListID:        b.cfg.ProjectClickUpListID,
		ClickUpNew:           clickup.New,
		LinearEnabled:        b.cfg.ProjectLinearEnabled,
		LinearAPIKey:         b.cfg.ProjectLinearAPIKey,
		LinearTeamKey:        b.cfg.ProjectLinearTeamKey,
		LinearNew:            linear.New,
		DeploysErrorsEnabled: b.cfg.ProjectDeploysErrorsEnabled,
		DeploysAPIToken:      b.cfg.ProjectDeploysAPIToken,
		DeploysBasicUser: func(project string) string {
			u, _ := b.cfg.ProjectDeploysBasicAuth(project)
			return u
		},
		DeploysBasicPass: func(project string) string {
			_, p := b.cfg.ProjectDeploysBasicAuth(project)
			return p
		},
		DeploysProject: func(project string) string {
			if d := b.cfg.ProjectDeploysErrors(project); d != nil {
				return d.Project
			}
			return ""
		},
		DeploysLocation: func(project string) string {
			if d := b.cfg.ProjectDeploysErrors(project); d != nil {
				return d.Location
			}
			return ""
		},
		DeploysDeployment: func(project string) string {
			if d := b.cfg.ProjectDeploysErrors(project); d != nil {
				return d.Deployment
			}
			return ""
		},
		DeploysNew:      deploys.New,
		SentryEnabled:   b.cfg.ProjectSentryEnabled,
		SentryAuthToken: b.cfg.ProjectSentryAuthToken,
		SentryOrg: func(project string) string {
			if c := b.cfg.ProjectSentry(project); c != nil {
				return c.Org
			}
			return ""
		},
		SentryProject: func(project string) string {
			if c := b.cfg.ProjectSentry(project); c != nil {
				return c.Project
			}
			return ""
		},
		SentryBaseURL: func(project string) string {
			if c := b.cfg.ProjectSentry(project); c != nil {
				return c.BaseURL
			}
			return ""
		},
		SentryNew:        sentry.New,
		GCPErrorsEnabled: b.cfg.ProjectGCPErrorsEnabled,
		GCPProjectID: func(project string) string {
			if c := b.cfg.ProjectGCPErrors(project); c != nil {
				return c.ProjectID
			}
			return ""
		},
		GCPProjectNumber: func(project string) string {
			if c := b.cfg.ProjectGCPErrors(project); c != nil {
				return c.ProjectNumber
			}
			return ""
		},
		GCPService: func(project string) string {
			if c := b.cfg.ProjectGCPErrors(project); c != nil {
				return c.Service
			}
			return ""
		},
		GCPCredentialsFile: func(project string) string {
			if c := b.cfg.ProjectGCPErrors(project); c != nil {
				return c.CredentialsFile
			}
			return ""
		},
		GCPNew: func(project string) *gcperr.Client {
			c := b.cfg.ProjectGCPErrors(project)
			if c == nil {
				return &gcperr.Client{}
			}
			return &gcperr.Client{
				ProjectID: c.ProjectID,
				Service:   c.Service,
				Tokens:    gcperr.TokenSourceFor(c.CredentialsFile),
			}
		},
		ListEligibleReviewers: func(project string) []agentapi.ReviewerRow {
			return b.listEligibleReviewers(project)
		},
		OnReviewRequested: func(req reviewstore.Request) {
			b.NotifyTeamReviewRequested(req)
		},
	}
	sock := agentmcp.ShortUnixPath(filepath.Join(b.cfg.DataDir, "agent.sock"))
	b.agent = &agentPlane{Auth: auth, API: api, Storage: storage, Bridge: &agentmcp.Bridge{Service: api}, Sock: sock}
}

func (b *Bot) startAgentBridge() {
	if b == nil || b.agent == nil || b.agent.Bridge == nil || b.agent.started {
		return
	}
	b.agent.started = true
	if err := b.agent.Bridge.ListenUnix(b.agent.Sock); err != nil {
		b.agent.started = false
		log.Printf("warn: agent mcp bridge: %v", err)
	}
}

// eligibleTeamReviewer mirrors web eligibleReviewer: on project (member or web
// admin) AND builder-class CanShip(). Web admin alone is not enough — under
// SafeTeamMode an admin not granted ship caps must not become a review assignee.
func (b *Bot) eligibleTeamReviewer(project, userID string) bool {
	if b == nil || b.cfg == nil || strings.TrimSpace(userID) == "" {
		return false
	}
	project = strings.TrimSpace(project)
	userID = strings.TrimSpace(userID)
	if project == "" {
		return false
	}
	if !b.reviewerOnProject(project, userID) {
		return false
	}
	return b.cfg.ResolveCapabilities(project, userID).CanShip()
}

// reviewerOnProject matches web/reviews.go: web admin or project member.
func (b *Bot) reviewerOnProject(project, userID string) bool {
	if b == nil || b.cfg == nil {
		return false
	}
	for _, id := range b.cfg.WebAuthAdminIDs() {
		if config.SameActor(id, userID) {
			return true
		}
	}
	return b.cfg.CanAccessProject(project, userID, config.WebRoleMember)
}

// listEligibleReviewers returns member ids that pass eligibleTeamReviewer.
// Eligibility is checked on the stored snapshot id; the emitted id is the
// same wire form Discord /review stores (identity.Canonical, Discord as a
// bare snowflake) so ListForReviewer == matches the signed-in account.
func (b *Bot) listEligibleReviewers(project string) []agentapi.ReviewerRow {
	if b == nil || b.cfg == nil {
		return nil
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	var members []string
	for _, p := range b.cfg.Snapshot().Projects {
		if strings.EqualFold(p.Name, project) {
			members = p.MemberIDs
			break
		}
	}
	out := make([]agentapi.ReviewerRow, 0, len(members))
	seen := map[string]struct{}{}
	for _, raw := range members {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !b.eligibleTeamReviewer(project, raw) {
			continue
		}
		id := b.canonicalActorID(raw)
		if config.IsDiscordActor(id) {
			id = config.ActorSubject(id)
		}
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		name := id
		if b.agent != nil && b.agent.API != nil && b.agent.API.DisplayName != nil {
			if n := strings.TrimSpace(b.agent.API.DisplayName(id)); n != "" {
				name = n
			}
		}
		out = append(out, agentapi.ReviewerRow{ID: id, Name: name})
	}
	slices.SortFunc(out, func(a, b agentapi.ReviewerRow) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// AgentAPI returns the agent control plane service (may be nil).
func (b *Bot) AgentAPI() *agentapi.Service {
	if b == nil || b.agent == nil {
		return nil
	}
	return b.agent.API
}

// mcpCapsForRun decides whether this run attaches grokwork MCP and which
// caps to mint. Grok investigate stays off unless always is set: --deny
// MCPTool is what blocks ambient user-scope MCP, and dropping it would
// attach every configured server. Claude can attach only our server via
// --mcp-config + --strict-mcp-config. cursor-agent has no --mcp-config, so
// it never attaches — writing .cursor/mcp.json into the worktree would
// pollute the session branch. always is the per-project AgentMCPAlways
// opt-in (trusted teams); tools-off still never attaches.
func mcpCapsForRun(agent grokrun.Agent, pol RunPolicy, always bool) (agentauth.Caps, bool) {
	if agent == grokrun.AgentCursor {
		return agentauth.Caps{}, false
	}
	if pol.Tools == nil {
		return agentauth.DefaultShipCaps(), true
	}
	if strings.TrimSpace(*pol.Tools) == "" {
		return agentauth.Caps{}, false
	}
	if agent == grokrun.AgentClaude || always {
		return agentauth.DefaultInvestigateCaps(), true
	}
	return agentauth.Caps{}, false
}

// prepareAgentMCP mints a session-bound token and writes MCP config outside
// the worktree. Ship/fix (any agent) and Claude investigate attach; Grok
// investigate attaches only when the project has AgentMCPAlways. Tools-off
// never attaches. Claude uses --mcp-config; grok uses the same server from
// user scope plus TOKEN/SOCK in the child env (and drops --deny MCPTool
// when this mint succeeds on an allowlisted run).
func (b *Bot) prepareAgentMCP(threadID, project, actorID string, agent grokrun.Agent, pol RunPolicy) (mcpPath, token string, ok bool) {
	if b == nil || b.agent == nil || b.cfg == nil || !b.cfg.AgentMCPEnabled() {
		return "", "", false
	}
	caps, attach := mcpCapsForRun(agent, pol, b.cfg.ProjectAgentMCPAlways(project))
	if !attach {
		return "", "", false
	}
	// Error-source catalog filter. Linear/ClickUp stay on the Default* set
	// (call still errors "not enabled") — do not change that in L1.
	if !b.cfg.ProjectGCPErrorsEnabled(project) {
		caps.GCPErrorsRead = false
	}
	if !b.cfg.ProjectSentryEnabled(project) {
		caps.SentryRead = false
	}
	if !b.cfg.ProjectDeploysErrorsEnabled(project) {
		caps.DeploysErrorsRead = false
	}
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(project) == "" {
		return "", "", false
	}
	b.startAgentBridge()
	ttl := time.Duration(b.cfg.TimeoutMsValue())*time.Millisecond + 5*time.Minute
	if ttl < 30*time.Minute {
		ttl = 45 * time.Minute
	}
	raw, cred, err := b.agent.Auth.Mint(threadID, project, actorID, "", caps, ttl)
	if err != nil {
		log.Printf("warn: agent mint: %v", err)
		return "", "", false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("warn: agent executable: %v", err)
		b.agent.Auth.Revoke(cred.ID)
		return "", "", false
	}
	scratch := filepath.Join(b.cfg.DataDir, "agent-mcp", threadID)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		b.agent.Auth.Revoke(cred.ID)
		return "", "", false
	}
	cfgPath := filepath.Join(scratch, "mcp.json")
	// Claude spawns the stdio server with this env map (parent env alone is not
	// enough for the child). Token is also in the agent child's env for any
	// direct use; same-UID residual is accepted (identity, not secrecy).
	doc := map[string]any{
		"mcpServers": map[string]any{
			"grokwork": map[string]any{
				"command": exe,
				"args":    []string{"agent-mcp-stdio"},
				"env": map[string]string{
					grokrun.AgentTokenEnv: raw,
					grokrun.AgentSockEnv:  b.agent.Sock,
				},
			},
		},
	}
	rawJSON, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		b.agent.Auth.Revoke(cred.ID)
		return "", "", false
	}
	if err := os.WriteFile(cfgPath, rawJSON, 0o600); err != nil {
		b.agent.Auth.Revoke(cred.ID)
		return "", "", false
	}
	return cfgPath, raw, true
}

func (b *Bot) revokeAgentThread(threadID string) {
	if b == nil || b.agent == nil {
		return
	}
	b.agent.Auth.RevokeThread(threadID)
}

func (b *Bot) agentMCPPromptForToken(tok string) string {
	if b == nil || b.agent == nil || b.agent.Auth == nil {
		return ""
	}
	cred, err := b.agent.Auth.Verify(tok)
	if err != nil {
		return ""
	}
	return agentMCPPromptContract(cred.Caps)
}

// agentMCPPromptContract is appended when MCP was actually attached.
func agentMCPPromptContract(caps agentauth.Caps) string {
	defs := agentmcp.ToolDefsFor(caps)
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		return ""
	}
	lines := []string{
		"You have grokwork MCP tools (server \"grokwork\"): " + strings.Join(names, ", ") + ".",
		"Use them for team workflow state; do not invent HTTP calls to the admin UI.",
	}
	if caps.ClickUpRead {
		lines = append(lines,
			"ClickUp refs (custom id, native id, or ClickUp URL) go to clickup_get_task — not issues_list, and not ClickUp's HTTP API.",
			"Do not invent ClickUp API keys. Do not change ClickUp status.",
		)
	}
	if caps.LinearRead {
		lines = append(lines,
			"Linear refs (TEAM-N or a Linear issue URL) go to linear_get_issue — not issues_list, and not Linear HTTP.",
			"Do not invent Linear API keys. Do not call Linear issueUpdate.",
		)
	}
	if caps.DeploysErrorsRead {
		lines = append(lines,
			"deploys.app error refs (id, location/name/id, or console URL) go to deploys_errors_get — not deploys.app HTTP.",
			"Do not invent deploys.app tokens. Do not resolve, mute, or assign.",
			"Do not paste full stacks or request payloads into Discord; summarize.",
		)
	}
	if caps.SentryRead {
		lines = append(lines,
			"Sentry refs (numeric id, short id, or Sentry URL) go to sentry_get_issue — not Sentry HTTP.",
			"Do not invent Sentry tokens or DSNs. Do not resolve, mute, or assign.",
			"Do not paste full stacks or request payloads into Discord; summarize.",
		)
	}
	if caps.GCPErrorsRead {
		lines = append(lines,
			"GCP Error Reporting refs (group id or Cloud Console URL) go to gcp_errors_get — not GCP HTTP.",
			"Do not invent GCP credentials. Do not resolve groups.",
			"Do not paste full stacks or request payloads into Discord; summarize.",
		)
	}
	if caps.SessionDone || caps.SessionAbandon {
		lines = append(lines,
			"When the task is complete you may call session_done (or session_abandon if giving up),",
			"and/or end with SESSION_DONE: / SESSION_ABANDON: markers.",
		)
	} else {
		lines = append(lines,
			"This run is read-only on grokwork workflow state: do not mark the session done or abandon it.",
		)
	}
	if caps.StorageRead {
		lines = append(lines,
			"Project storage is shared across sessions in this project — use clear keys (e.g. reports/<case>/…).",
		)
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// ShutdownAgentPlane stops the UDS bridge (optional at process exit).
func (b *Bot) ShutdownAgentPlane(ctx context.Context) {
	if b == nil || b.agent == nil || b.agent.Bridge == nil {
		return
	}
	_ = b.agent.Bridge.Shutdown(ctx)
	if sock := strings.TrimSpace(b.agent.Sock); sock != "" {
		_ = os.Remove(sock)
	}
}

// AgentSockPath is the UDS path for the agent bridge (tests / stdio child).
func (b *Bot) AgentSockPath() string {
	if b == nil || b.agent == nil {
		return ""
	}
	return b.agent.Sock
}

// Ensure agentPlane field is not nil when minting in tests.
func (b *Bot) ensureAgentPlaneForTest() error {
	if b.agent != nil {
		return nil
	}
	if b.cfg == nil || b.cfg.DataDir == "" {
		return fmt.Errorf("no data dir")
	}
	b.initAgentPlane()
	if b.agent == nil {
		return fmt.Errorf("agent plane init failed")
	}
	return nil
}
