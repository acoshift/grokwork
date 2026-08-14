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
		GH:                 b.ghRunner,
		ClickUpEnabled:     b.cfg.ProjectClickUpEnabled,
		ClickUpAPIKey:      b.cfg.ProjectClickUpAPIKey,
		ClickUpWorkspaceID: b.cfg.ProjectClickUpWorkspaceID,
		ClickUpListID:      b.cfg.ProjectClickUpListID,
		ClickUpNew:         clickup.New,
		LinearEnabled:      b.cfg.ProjectLinearEnabled,
		LinearAPIKey:       b.cfg.ProjectLinearAPIKey,
		LinearTeamKey:      b.cfg.ProjectLinearTeamKey,
		LinearNew:          linear.New,
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

// prepareAgentMCP mints a session-bound token and writes MCP config outside
// the worktree when the run is unrestricted (investigate/tools-off excluded).
// Claude uses the file via --mcp-config; grok uses the same server from user
// scope plus TOKEN/SOCK in the child env. Returns path, raw token, attached.
func (b *Bot) prepareAgentMCP(threadID, project, actorID string, agent grokrun.Agent, pol RunPolicy) (mcpPath, token string, ok bool) {
	if b == nil || b.agent == nil || b.cfg == nil || !b.cfg.AgentMCPEnabled() {
		return "", "", false
	}
	// Unrestricted tools only (investigate/tools-off must not get MCP).
	if pol.Tools != nil {
		return "", "", false
	}
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(project) == "" {
		return "", "", false
	}
	b.startAgentBridge()
	ttl := time.Duration(b.cfg.TimeoutMsValue())*time.Millisecond + 5*time.Minute
	if ttl < 30*time.Minute {
		ttl = 45 * time.Minute
	}
	raw, cred, err := b.agent.Auth.Mint(threadID, project, actorID, "", agentauth.DefaultShipCaps(), ttl)
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

// agentMCPPromptContract is appended when MCP was actually attached.
func agentMCPPromptContract() string {
	return strings.Join([]string{
		"You have grokwork MCP tools (server \"grokwork\"): session_get, session_done, session_abandon,",
		"prs_list, issues_list, review_request, reviewers_list, storage_put, storage_get, storage_list, storage_delete,",
		"clickup_get_task, clickup_list_tasks, linear_get_issue, linear_list_issues.",
		"Use them for team workflow state; do not invent HTTP calls to the admin UI.",
		"ClickUp refs (custom id, native id, or ClickUp URL) go to clickup_get_task — not issues_list, and not ClickUp's HTTP API.",
		"Do not invent ClickUp API keys. Do not change ClickUp status.",
		"Linear refs (TEAM-N or a Linear issue URL) go to linear_get_issue — not issues_list, and not Linear HTTP.",
		"Do not invent Linear API keys. Do not call Linear issueUpdate.",
		"When the task is complete you may call session_done (or session_abandon if giving up),",
		"and/or end with SESSION_DONE: / SESSION_ABANDON: markers.",
		"Project storage is shared across sessions in this project — use clear keys (e.g. reports/<case>/…).",
		"",
	}, "\n")
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
