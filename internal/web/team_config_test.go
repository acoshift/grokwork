package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

// postForm posts a form to the auth-off test server and fails on a non-redirect.
func postTeamForm(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("POST %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w
}

// getBody GETs a page off the auth-off test server and returns its body.
func getTeamBody(t *testing.T, srv *Server, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestProjectTeamRosterCRUD drives the whole team lifecycle through the four
// HTTP routes and checks each step against config, the rendered Access tab, and
// the audit log. Team membership is the grant, so the capability assertions are
// part of CRUD, not a separate concern.
func TestProjectTeamRosterCRUD(t *testing.T) {
	srv, cfg, _ := testServer(t)

	// Create.
	postTeamForm(t, srv, "/config/projects/teams", url.Values{
		"name": {"proj"}, "key": {"Support"}, "label": {"Support"}, "capabilities": {"investigator"},
	})
	teams := cfg.ProjectTeams("proj")
	// The key is normalized: routes address a team by a lowercased id.
	tm, ok := teams["support"]
	if !ok {
		t.Fatalf("team not created: %+v", teams)
	}
	if tm.Label != "Support" || tm.Capabilities != "investigator" {
		t.Fatalf("team=%+v", tm)
	}
	if len(tm.Members) != 0 {
		t.Fatalf("new team must start empty: %+v", tm)
	}

	// A brand-new team grants nothing, so the project is still fail-closed.
	body := getTeamBody(t, srv, "/config/projects/proj")
	for _, want := range []string{`id="team-roster"`, "Support", "investigator"} {
		if !strings.Contains(body, want) {
			t.Fatalf("access tab missing %q", want)
		}
	}
	if !strings.Contains(body, "this team grants nothing yet") {
		t.Fatalf("member-less team must be flagged: %s", body)
	}

	// Add member — namespaced id, stored normalized.
	postTeamForm(t, srv, "/config/projects/teams/members", url.Values{
		"name": {"proj"}, "key": {"support"}, "id": {"discord:sup-1"},
	})
	if !cfg.AccessAllowed("proj", "sup-1") {
		t.Fatal("team member has no project access (bare id must match discord: form)")
	}
	caps := cfg.ResolveCapabilities("proj", "sup-1")
	if !caps.FileEscalation {
		t.Fatalf("team member did not inherit investigator: %+v", caps)
	}
	if caps.CanShip() {
		t.Fatalf("investigator must not be builder-class: %+v", caps)
	}
	body = getTeamBody(t, srv, "/config/projects/proj")
	if !strings.Contains(body, "discord:sup-1") {
		t.Fatalf("roster missing team member: %s", body)
	}

	// Change the team's capability template — every member's grant moves with it.
	postTeamForm(t, srv, "/config/projects/teams", url.Values{
		"name": {"proj"}, "key": {"support"}, "label": {"Support & Success"}, "capabilities": {"builder"},
	})
	if !cfg.ResolveCapabilities("proj", "sup-1").CanShip() {
		t.Fatal("template change did not reach the existing member")
	}
	if got := cfg.ProjectTeams("proj")["support"]; len(got.Members) != 1 || got.Label != "Support & Success" {
		t.Fatalf("editing label/template must keep members: %+v", got)
	}

	// An unknown template is refused rather than silently stored (which would
	// downgrade every member to the fallback without saying so).
	req := httptest.NewRequest(http.MethodPost, "/config/projects/teams", strings.NewReader(url.Values{
		"name": {"proj"}, "key": {"support"}, "capabilities": {"buildr"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("unknown template must redirect with err=, got %q", loc)
	}
	if cfg.ProjectTeams("proj")["support"].Capabilities != "builder" {
		t.Fatalf("refused write must not change the team: %+v", cfg.ProjectTeams("proj")["support"])
	}

	// Remove member.
	postTeamForm(t, srv, "/config/projects/teams/members/remove", url.Values{
		"name": {"proj"}, "key": {"support"}, "id": {"sup-1"},
	})
	if cfg.AccessAllowed("proj", "sup-1") {
		t.Fatal("removed team member still has access")
	}

	// Remove team.
	postTeamForm(t, srv, "/config/projects/teams/remove", url.Values{
		"name": {"proj"}, "key": {"support"},
	})
	if _, ok := cfg.ProjectTeams("proj")["support"]; ok {
		t.Fatalf("team not removed: %+v", cfg.ProjectTeams("proj"))
	}

	// Every mutation is audited, matching the neighbouring project-config POSTs.
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		audit.ActionConfigSetTeam,
		audit.ActionConfigAddTeamMember,
		audit.ActionConfigRemoveTeamMember,
		audit.ActionConfigRemoveTeam,
	} {
		var found bool
		for _, ev := range evs {
			if ev.Action == want && ev.OK && ev.Detail["project"] == "proj" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing audit action %q in %+v", want, evs)
		}
	}
	// The refused write is audited too, as a failure.
	var sawFail bool
	for _, ev := range evs {
		if ev.Action == audit.ActionConfigSetTeam && !ev.OK {
			sawFail = true
			break
		}
	}
	if !sawFail {
		t.Fatalf("refused team write was not audited as a failure: %+v", evs)
	}
}

// TestProjectAccessTabCountsTeamMembers pins the two places that used to count
// allowedUserIds + allowedRoleIds: the Access tab badge and the fail-closed
// banner. A project whose only grant is a team is NOT fail-closed.
func TestProjectAccessTabCountsTeamMembers(t *testing.T) {
	srv, cfg, _ := testServer(t)
	// Drop the seeded direct member so the team is the only grant.
	if err := cfg.RemoveProjectAllowedUser("proj", "u0"); err != nil {
		t.Fatal(err)
	}
	body := getTeamBody(t, srv, "/config/projects/proj")
	if !strings.Contains(body, "fail-closed") {
		t.Fatalf("no members at all must show the fail-closed banner: %s", body)
	}

	if err := cfg.SetProjectTeam("proj", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	// A team with no members is still no allowlist.
	body = getTeamBody(t, srv, "/config/projects/proj")
	if !strings.Contains(body, "fail-closed") {
		t.Fatalf("member-less team must stay fail-closed: %s", body)
	}

	if err := cfg.AddProjectTeamMember("proj", "eng", "discord:eng-1"); err != nil {
		t.Fatal(err)
	}
	body = getTeamBody(t, srv, "/config/projects/proj")
	if strings.Contains(body, "fail-closed") {
		t.Fatalf("team member must clear the fail-closed banner: %s", body)
	}
	if !strings.Contains(body, `>Access <span class="count">1</span>`) {
		t.Fatalf("Access tab count must include team members: %s", body)
	}
	// The config hub row reads the same union. Scope the check to the project's
	// own row — the page's help copy also contains the words "no members".
	hub := getTeamBody(t, srv, "/config")
	i := strings.Index(hub, `<a class="drill" href="/config/projects/proj">`)
	if i < 0 {
		t.Fatalf("config hub has no project row: %s", hub)
	}
	row := hub[i:]
	if j := strings.Index(row, "</a>"); j >= 0 {
		row = row[:j]
	}
	if !strings.Contains(row, "1 member") || strings.Contains(row, "no members") {
		t.Fatalf("config hub row must count team members: %s", row)
	}
	if strings.Contains(row, "fail-closed") {
		t.Fatalf("config hub row must not flag a team-only project: %s", row)
	}
}

// TestTeamOnlyMemberOpensProject covers the whole web read path for someone
// whose only grant is a team: they must be able to log in as a member, see the
// project on the launcher, and open the workspace.
func TestTeamOnlyMemberOpensProject(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectTeam("proj", "support", "Support", "investigator"); err != nil {
		t.Fatal(err)
	}
	// team-1 is on no allowlist and in no capability map — the team is all of it.
	if err := cfg.AddProjectTeamMember("proj", "support", "team-1"); err != nil {
		t.Fatal(err)
	}

	// Login itself is a membership decision when web auth derives roles from
	// project membership, so assert the resolver too.
	if role, ok := cfg.ResolveWebRoleForConfig("team-1"); !ok || role != config.WebRoleMember {
		t.Fatalf("team-only member cannot log in: role=%q ok=%v", role, ok)
	}

	sid, _, err := srv.LoginAs("team-1", "Teamer", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	home := get("/", sid)
	if home.Code != http.StatusOK {
		t.Fatalf("launcher status=%d", home.Code)
	}
	if !strings.Contains(home.Body.String(), "/projects/proj") {
		t.Fatalf("launcher hides the project from its team member: %s", home.Body.String())
	}
	if ws := get("/projects/proj", sid); ws.Code != http.StatusOK {
		t.Fatalf("workspace status=%d body=%s", ws.Code, ws.Body.String())
	}

	// Fail-closed control: a member on no team and no allowlist sees nothing.
	otherSID, _, err := srv.LoginAs("stranger-1", "Stranger", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if home := get("/", otherSID); strings.Contains(home.Body.String(), "/projects/proj") {
		t.Fatalf("launcher leaked the project to a non-member: %s", home.Body.String())
	}
	if ws := get("/projects/proj", otherSID); ws.Code == http.StatusOK {
		t.Fatalf("non-member opened the workspace: %d", ws.Code)
	}
}

// TestTeamMemberIsEligibleReviewer pins the reviewer dropdown against the bug
// the team model exposes: it read AllowedUserIDs, so a builder who was only on
// the project through a team could never be asked for a review.
func TestTeamMemberIsEligibleReviewer(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectTeam("proj", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProjectTeamMember("proj", "eng", "discord:eng-1"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectTeam("proj", "support", "Support", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProjectTeamMember("proj", "support", "sup-1"); err != nil {
		t.Fatal(err)
	}

	if !srv.canRequestReviewer("proj", "eng-1") {
		t.Fatal("team-only builder is not an eligible reviewer")
	}
	if srv.canRequestReviewer("proj", "sup-1") {
		t.Fatal("operator team must not be reviewer-eligible (not builder-class)")
	}

	opts := srv.reviewerOptions("proj")
	ids := make(map[string]bool, len(opts))
	for _, o := range opts {
		ids[o.ID] = true
	}
	// The option id is the bare snowflake, not "discord:eng-1": reviewstore
	// compares reviewer ids verbatim and the GitHub-login map is snowflake-keyed.
	if !ids["eng-1"] {
		t.Fatalf("reviewer dropdown missing the team-only builder: %+v", opts)
	}
	if ids["discord:eng-1"] {
		t.Fatalf("reviewer id must be the bare subject: %+v", opts)
	}
	if ids["sup-1"] {
		t.Fatalf("reviewer dropdown offered a non-builder: %+v", opts)
	}
}

// TestReviewerAdminIDAcceptsNamespaced covers the one raw-string actor compare
// left in the web layer: webAuth.adminDiscordIds written as "discord:<id>" has
// to match a runtime bare snowflake, like containsID does everywhere else.
func TestReviewerAdminIDAcceptsNamespaced(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{AdminDiscordIDs: []string{"discord:root-1"}}
	if !srv.reviewerOnProject("proj", "root-1") {
		t.Fatal(`admin id "discord:root-1" did not match runtime id "root-1"`)
	}
	if srv.reviewerOnProject("proj", "root-2") {
		t.Fatal("unrelated id matched the admin list")
	}
}

// TestConfigDigestTracksTeams: the config page auto-refreshes off fpConfig, so a
// team edit that leaves the digest unchanged makes the Access tab go stale.
func TestConfigDigestTracksTeams(t *testing.T) {
	srv, cfg, _ := testServer(t)
	base := srv.fpConfig()

	if err := cfg.SetProjectTeam("proj", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	afterCreate := srv.fpConfig()
	if afterCreate == base {
		t.Fatal("creating a team did not move the config digest")
	}

	if err := cfg.AddProjectTeamMember("proj", "eng", "eng-1"); err != nil {
		t.Fatal(err)
	}
	afterMember := srv.fpConfig()
	if afterMember == afterCreate {
		t.Fatal("adding a team member did not move the config digest")
	}

	// A rename changes no membership, so MemberIDs alone would not notice it.
	if err := cfg.SetProjectTeam("proj", "eng", "Platform", "builder"); err != nil {
		t.Fatal(err)
	}
	if srv.fpConfig() == afterMember {
		t.Fatal("renaming a team did not move the config digest")
	}
}
