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
	// Unrestricted Claude → MCP ok
	path, tok, ok := b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentClaude, RunPolicy{})
	if !ok || path == "" || tok == "" {
		t.Fatalf("expected mcp path=%q tok empty=%v ok=%v", path, tok == "", ok)
	}
	// Investigate tools → no MCP
	tools := "read_file,grep"
	_, _, ok = b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentClaude, RunPolicy{Tools: &tools})
	if ok {
		t.Fatal("investigate must not get MCP")
	}
	// Grok unrestricted → MCP (this host's default agent)
	gpath, gtok, ok := b.prepareAgentMCP("t1", "app", "actor", grokrun.AgentGrok, RunPolicy{})
	if !ok || gpath == "" || gtok == "" {
		t.Fatalf("grok expected mcp path=%q tok empty=%v ok=%v", gpath, gtok == "", ok)
	}
	b.revokeAgentThread("t1")
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
