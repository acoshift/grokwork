package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPrepareAgentMCPClaudeOnly(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.ensureAgentPlaneForTest(); err != nil {
		// testFixBot may not set DataDir the same way — init via New path.
		t.Skip(err.Error())
	}
	// Unrestricted Claude → MCP ok (ship caps)
	path, tok, ok := b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentClaude, RunPolicy{})
	if !ok || path == "" || tok == "" {
		t.Fatalf("expected mcp path=%q tok empty=%v ok=%v", path, tok == "", ok)
	}
	cred, err := b.agent.Auth.Verify(tok)
	if err != nil || !cred.Caps.SessionDone {
		t.Fatalf("ship token should allow session_done: %+v %v", cred.Caps, err)
	}
	// Claude investigate → read-only MCP
	tools := "Read,Grep,Glob"
	ipath, itok, iok := b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentClaude, RunPolicy{Tools: &tools})
	if !iok || ipath == "" || itok == "" {
		t.Fatal("claude investigate must get MCP")
	}
	icred, err := b.agent.Auth.Verify(itok)
	if err != nil {
		t.Fatal(err)
	}
	if !icred.Caps.SessionRead || icred.Caps.SessionDone || icred.Caps.StorageWrite || icred.Caps.ReviewRequest {
		t.Fatalf("investigate caps=%+v", icred.Caps)
	}
	// Grok investigate → still no MCP (--deny MCPTool)
	gtools := "read_file,grep"
	_, _, ok = b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentGrok, RunPolicy{Tools: &gtools})
	if ok {
		t.Fatal("grok investigate must not get MCP")
	}
	// Tools-off / explain → no MCP
	empty := ""
	_, _, ok = b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentClaude, RunPolicy{Tools: &empty})
	if ok {
		t.Fatal("tools-off must not get MCP")
	}
	// Grok unrestricted → MCP (this host's default agent)
	gpath, gtok, ok := b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentGrok, RunPolicy{})
	if !ok || gpath == "" || gtok == "" {
		t.Fatalf("grok expected mcp path=%q tok empty=%v ok=%v", gpath, gtok == "", ok)
	}
	b.revokeAgentThread("t1")
}

func TestPrepareAgentMCPAlwaysAttachesGrokInvestigate(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.ensureAgentPlaneForTest(); err != nil {
		t.Skip(err.Error())
	}
	if err := b.cfg.SetProjectAgentMCPAlways("app", true); err != nil {
		t.Fatal(err)
	}
	gtools := "read_file,grep"
	path, tok, ok := b.prepareAgentMCP("t-always", "app", "actor", grokrun.AgentGrok, RunPolicy{Tools: &gtools})
	if !ok || path == "" || tok == "" {
		t.Fatal("grok investigate must get MCP when agentMCPAlways is set")
	}
	cred, err := b.agent.Auth.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Caps.SessionRead || cred.Caps.SessionDone || cred.Caps.StorageWrite {
		t.Fatalf("always-attach grok investigate must stay read-only: %+v", cred.Caps)
	}
	empty := ""
	_, _, ok = b.prepareAgentMCP("t-always", "app", "actor", grokrun.AgentGrok, RunPolicy{Tools: &empty})
	if ok {
		t.Fatal("tools-off must not get MCP even when always is set")
	}
	_, _, ok = b.prepareAgentMCP("t-always", "app", "actor", grokrun.AgentCursor, RunPolicy{})
	if ok {
		t.Fatal("cursor must not get MCP even when always is set")
	}
	b.revokeAgentThread("t-always")
}

func TestMCPCapsForRun(t *testing.T) {
	t.Parallel()
	if _, ok := mcpCapsForRun(grokrun.AgentGrok, RunPolicy{}, false); !ok {
		t.Fatal("grok ship")
	}
	tools := "Read,Grep"
	caps, ok := mcpCapsForRun(grokrun.AgentClaude, RunPolicy{Tools: &tools}, false)
	if !ok || caps.SessionDone || !caps.SessionRead {
		t.Fatalf("claude investigate: %+v ok=%v", caps, ok)
	}
	if _, ok := mcpCapsForRun(grokrun.AgentGrok, RunPolicy{Tools: &tools}, false); ok {
		t.Fatal("grok investigate")
	}
	gCaps, gOK := mcpCapsForRun(grokrun.AgentGrok, RunPolicy{Tools: &tools}, true)
	if !gOK || gCaps.SessionDone || !gCaps.SessionRead {
		t.Fatalf("grok investigate always: %+v ok=%v", gCaps, gOK)
	}
	empty := ""
	if _, ok := mcpCapsForRun(grokrun.AgentClaude, RunPolicy{Tools: &empty}, true); ok {
		t.Fatal("tools-off")
	}
	if _, ok := mcpCapsForRun(grokrun.AgentCursor, RunPolicy{}, true); ok {
		t.Fatal("cursor-agent has no --mcp-config")
	}
	if _, ok := mcpCapsForRun(grokrun.AgentCursor, RunPolicy{Tools: &tools}, true); ok {
		t.Fatal("cursor investigate")
	}
}

func TestPrepareAgentMCPStripsDisabledErrorCaps(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.ensureAgentPlaneForTest(); err != nil {
		t.Fatal(err)
	}
	path, tok, ok := b.prepareAgentMCP("t-strip", "app", "actor", grokrun.AgentClaude, RunPolicy{})
	if !ok || path == "" || tok == "" {
		t.Fatal("expected mint")
	}
	cred, err := b.agent.Auth.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Caps.DeploysErrorsRead || cred.Caps.SentryRead || cred.Caps.GCPErrorsRead {
		t.Fatalf("disabled providers must be stripped: %+v", cred.Caps)
	}
	if !cred.Caps.LinearRead || !cred.Caps.ClickUpRead {
		t.Fatalf("must not strip Linear/ClickUp: %+v", cred.Caps)
	}
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	_, tok2, ok := b.prepareAgentMCP("t-strip2", "app", "actor", grokrun.AgentClaude, RunPolicy{})
	if !ok {
		t.Fatal("expected mint")
	}
	cred2, err := b.agent.Auth.Verify(tok2)
	if err != nil {
		t.Fatal(err)
	}
	if !cred2.Caps.DeploysErrorsRead {
		t.Fatal("enabled deploys must keep DeploysErrorsRead")
	}
	if cred2.Caps.SentryRead || cred2.Caps.GCPErrorsRead {
		t.Fatalf("other error caps still on: %+v", cred2.Caps)
	}
	b.revokeAgentThread("t-strip")
	b.revokeAgentThread("t-strip2")
}

func TestAgentMCPPromptContractDeploys(t *testing.T) {
	t.Parallel()
	p := agentMCPPromptContract(agentauth.DefaultShipCaps())
	for _, want := range []string{"deploys_errors_get", "not deploys.app HTTP", "Do not invent deploys.app tokens", "Do not resolve, mute"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestAgentMCPPromptContractInvestigateOmitsWrites(t *testing.T) {
	t.Parallel()
	p := agentMCPPromptContract(agentauth.DefaultInvestigateCaps())
	for _, name := range []string{"session_get", "session_send_file", "prs_list", "clickup_get_task", "linear_get_issue", "storage_get"} {
		if !strings.Contains(p, name) {
			t.Fatalf("missing %q in %s", name, p)
		}
	}
	for _, name := range []string{"session_done", "session_abandon", "review_request", "storage_put", "storage_delete"} {
		if strings.Contains(p, name) {
			t.Fatalf("investigate prompt must not name %q:\n%s", name, p)
		}
	}
	if !strings.Contains(p, "read-only") {
		t.Fatalf("want read-only note:\n%s", p)
	}
}

func TestEligibleTeamReviewerBuilder(t *testing.T) {
	b, _ := testFixBot(t)
	// Without membership, not eligible.
	if b.eligibleTeamReviewer("app", "nobody") {
		t.Fatal("unknown user must not be eligible")
	}

	// Match web: team builder is eligible; operator is not.
	if err := b.cfg.SetProjectTeam("app", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.AddProjectTeamMember("app", "eng", "eng-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.SetProjectTeam("app", "support", "Support", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.AddProjectTeamMember("app", "support", "sup-1"); err != nil {
		t.Fatal(err)
	}
	if !b.eligibleTeamReviewer("app", "eng-1") {
		t.Fatal("team builder must be eligible")
	}
	if b.eligibleTeamReviewer("app", "sup-1") {
		t.Fatal("operator team must not be reviewer-eligible")
	}

	// Web admin on the project list still requires CanShip — same as web eligibleReviewer.
	// SafeTeamMode + unmapped admin → investigator default → !CanShip → ineligible.
	if err := b.cfg.SetProjectSafeTeam("app", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	b.cfg.WebAuth = &config.WebAuthConfig{
		Enabled:         true,
		AdminDiscordIDs: []string{"admin-only"},
	}
	if !b.reviewerOnProject("app", "admin-only") {
		t.Fatal("admin must count as on-project for reviewerOnProject")
	}
	if b.cfg.ResolveCapabilities("app", "admin-only").CanShip() {
		t.Fatal("precondition: unmapped admin under SafeTeamMode must not CanShip")
	}
	if b.eligibleTeamReviewer("app", "admin-only") {
		t.Fatal("admin without CanShip must not be eligible (must match web eligibleReviewer)")
	}

	rows := b.listEligibleReviewers("app")
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
		if r.Name == "" {
			t.Fatalf("empty name on %+v", r)
		}
	}
	// Snapshot MemberIDs are discord:eng-1; emit the wire form /review stores.
	if !ids["eng-1"] {
		t.Fatalf("builder missing (want wire form eng-1): %+v", rows)
	}
	if ids["discord:eng-1"] {
		t.Fatalf("must not emit discord: prefix: %+v", rows)
	}
	if ids["discord:sup-1"] || ids["sup-1"] {
		t.Fatalf("operator listed: %+v", rows)
	}
	if ids["discord:admin-only"] || ids["admin-only"] {
		t.Fatalf("admin without CanShip listed: %+v", rows)
	}
}

func TestListEligibleReviewersEmitsCanonicalAccount(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.cfg.SetProjectTeam("app", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.AddProjectTeamMember("app", "eng", "google:alice"); err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Link("999888777666555444", "google:alice", ""); err != nil {
		t.Fatal(err)
	}
	b.SetIdentity(ids)
	rows := b.listEligibleReviewers("app")
	if len(rows) != 1 || rows[0].ID != "google:alice" {
		t.Fatalf("want google:alice account id, got %+v", rows)
	}
}

func TestAgentAPISessionBoundViaBot(t *testing.T) {
	b, _ := testFixBot(t)
	if b.agent == nil {
		// testFixBot may not call full New with DataDir agent plane.
		if err := b.ensureAgentPlaneForTest(); err != nil {
			// Seed DataDir on cfg
			if b.cfg != nil && b.cfg.DataDir == "" {
				b.cfg.DataDir = t.TempDir()
			}
			b.initAgentPlane()
		}
	}
	if b.agent == nil {
		t.Skip("no agent plane")
	}
	_ = b.sessions.Set("t1", sessionstore.Entry{Project: "app", Goal: "g"})
	raw, _, err := b.agent.Auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := b.agent.API.SessionGet(raw)
	if err != nil || info.Goal != "g" {
		t.Fatalf("%v %+v", err, info)
	}
	// Foreign thread binding enforced by mint.
	if info.ThreadID != "t1" {
		t.Fatal(info.ThreadID)
	}
	if strings.Contains(info.ThreadID, "t2") {
		t.Fatal("wrong")
	}
}
