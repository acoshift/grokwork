package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
)

// seedDeployRun writes one run straight into the engine's store. The board is a
// pure read over that store, so no engine needs to run.
func seedDeployRun(t *testing.T, srv *Server, r deploy.Run) deploy.Run {
	t.Helper()
	if r.ID == "" {
		r.ID = deploy.NewRunID()
	}
	if r.Repo == "" {
		r.Repo = "acme/app"
	}
	if err := srv.Deploys().Store().Create(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// touchDeployRun pins a run record's mtime. LaneStates scans newest-modified
// first, so this is how a test decides which records fall inside a clipped
// window instead of leaving it to the filesystem's timestamp resolution.
func touchDeployRun(t *testing.T, srv *Server, id string, mod time.Time) {
	t.Helper()
	p := filepath.Join(srv.Deploys().Store().Dir(), id+".json")
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestDeploysBoardListsCurrentPerLane is the board's whole contract: one row
// per service+environment naming the commit that is actually live. The
// superseded predecessor must not appear as live anywhere — a board that names
// a replaced commit is worse than no board.
func TestDeploysBoardListsCurrentPerLane(t *testing.T) {
	srv, _, _ := testServer(t)

	old := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "aaa1111deadbeef", ShortSHA: "aaa1111", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
		ActorName: "Old Operator", SupersededBy: "later",
	})
	live := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "bbb2222deadbeef", ShortSHA: "bbb2222", Subject: "raise the pool size",
		Status:   deploy.StatusSucceeded,
		QueuedAt: "2026-07-02T00:00:00Z", EndedAt: "2026-07-02T00:05:00Z",
		ActorName: "Dana Ops",
	})
	// A second lane that has only ever failed: it must still be listed, and it
	// must not borrow the other lane's commit.
	broke := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "web", Env: "dev",
		SHA: "ccc3333deadbeef", ShortSHA: "ccc3333", Status: deploy.StatusFailed,
		QueuedAt: "2026-07-02T01:00:00Z", EndedAt: "2026-07-02T01:01:00Z",
		ActorName: "Dana Ops",
	})

	body := getBody(t, srv.Handler(), "/deploys")
	for _, want := range []string{
		`id="page-deploys-board"`,
		// One row per lane, keyed by service and environment.
		">api<", ">web<", ">prod<", ">dev<",
		// The live commit, its author and a link to the run that put it there.
		"bbb2222",
		"raise the pool size",
		"Dana Ops",
		`href="/projects/proj/deploys/` + live.ID + `"`,
		// The failed-only lane is reported as such, not omitted.
		"never deployed",
		`href="/projects/proj/deploys/` + broke.ID + `"`,
		// Live region on the deploy SSE domain, per the shell contract.
		`id="live-deploys-board"`,
		`hx-trigger="sse:deploy"`,
		`class="live-region"`,
		`hx-target="this"`,
		`hx-select="unset"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("deploy board missing %q", want)
		}
	}
	// The superseded run is history, not state: neither its commit nor a link
	// to it may appear on a board whose only claim is "this is what is running".
	if strings.Contains(body, "aaa1111") {
		t.Fatal("board rendered the superseded commit as current")
	}
	if strings.Contains(body, old.ID) {
		t.Fatal("board linked the superseded run")
	}
	// A failed newest run on a healthy lane is the board's other job; here the
	// failing lane has no success at all, so it counts as needing attention.
	if !strings.Contains(body, "Needs attention") {
		t.Fatal("board lost its attention counter")
	}
}

// TestDeploysBoardShowsFailureOnTopOfLiveDeploy pins the two-part answer a lane
// can have: the commit still running, plus the newer attempt that did not
// replace it. Collapsing those into one status is how a board ends up claiming
// prod is broken when it is merely un-upgraded.
func TestDeploysBoardShowsFailureOnTopOfLiveDeploy(t *testing.T) {
	srv, _, _ := testServer(t)
	seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "111aaaa", ShortSHA: "111aaaa", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
		ActorName: "Dana Ops",
	})
	failed := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "222bbbb", ShortSHA: "222bbbb", Status: deploy.StatusFailed,
		QueuedAt: "2026-07-02T00:00:00Z", EndedAt: "2026-07-02T00:01:00Z",
		ActorName: "Dana Ops",
	})
	body := getBody(t, srv.Handler(), "/deploys")
	// Still live at the older commit…
	if !strings.Contains(body, "111aaaa") {
		t.Fatal("a failed newer run must not unseat the live commit")
	}
	// …and the failure is surfaced next to it.
	if !strings.Contains(body, `href="/projects/proj/deploys/`+failed.ID+`"`) {
		t.Fatalf("failed newest run not linked:\n%s", body)
	}
	if !strings.Contains(body, ">failed</span>") {
		t.Fatal("failed status badge missing")
	}
	// The failed run's commit is not what is running, so it must not appear in
	// the Commit column.
	if strings.Contains(body, "222bbbb") {
		t.Fatal("board showed the failed attempt's commit as the live one")
	}
}

// TestDeploysBoardHidesInvisibleProjects is the ACL test that matters most for
// this page: it aggregates every project at once, so a missed filter leaks not
// just deploy activity but the existence and naming of other teams' projects.
func TestDeploysBoardHidesInvisibleProjects(t *testing.T) {
	srv := twoProjectAuthServer(t)
	seedDeployRun(t, srv, deploy.Run{
		Project: "public", Service: "api", Env: "prod",
		SHA: "pub1234", ShortSHA: "pub1234", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
		ActorName: "Public Operator",
	})
	seedDeployRun(t, srv, deploy.Run{
		Project: "secret", Service: "vault", Env: "prod",
		SHA: "sec9876", ShortSHA: "sec9876", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
		ActorName: "Secret Operator",
	})

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, sessionID string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	body := get("/deploys", sid)
	if !strings.Contains(body, "pub1234") || !strings.Contains(body, "Public Operator") {
		t.Fatalf("member lost their own project's lane:\n%s", body)
	}
	for _, leak := range []string{"sec9876", "Secret Operator", `href="/projects/secret`, ">vault<"} {
		if strings.Contains(body, leak) {
			t.Fatalf("board leaked %q to a non-member", leak)
		}
	}
	// The live region re-renders the same rows on every SSE tick, so it has to
	// be filtered too — an unfiltered partial leaks two seconds later.
	req := httptest.NewRequest(http.MethodGet, "/partials/deploys/board", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	partial := w.Body.String()
	if !strings.Contains(partial, "pub1234") {
		t.Fatal("partial lost the visible lane")
	}
	if strings.Contains(partial, "sec9876") || strings.Contains(partial, ">vault<") {
		t.Fatalf("live partial leaked a hidden project:\n%s", partial)
	}

	// An admin sees every configured project, which is what makes the filter a
	// visibility rule rather than a bug that happens to hide rows.
	adminSID, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	adminBody := get("/deploys", adminSID)
	if !strings.Contains(adminBody, "sec9876") || !strings.Contains(adminBody, "pub1234") {
		t.Fatalf("admin board is missing a project:\n%s", adminBody)
	}
}

// TestDeploysBoardDropsUnconfiguredProjects covers the run whose project was
// removed from config. ProjectsVisibleTo is applied as an allowlist, so leftover
// history on disk cannot resurrect a project name for anyone — including the
// admin, whose visibility set is "every configured project", not "everything".
func TestDeploysBoardDropsUnconfiguredProjects(t *testing.T) {
	srv, _, _ := testServer(t)
	seedDeployRun(t, srv, deploy.Run{
		Project: "deleted-project", Service: "ghost", Env: "prod",
		SHA: "gho5t00", ShortSHA: "gho5t00", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
	})
	body := getBody(t, srv.Handler(), "/deploys")
	for _, leak := range []string{"gho5t00", "deleted-project", ">ghost<"} {
		if strings.Contains(body, leak) {
			t.Fatalf("board rendered %q for a project that is no longer configured", leak)
		}
	}
}

// TestDeploysBoardNavIsGlobal pins the shell contract: /deploys is a lead view
// beside Ship/Sessions/Worktrees, so the sidebar renders in global mode and the
// Deploys anchor is the active one (bare-label convention).
func TestDeploysBoardNavIsGlobal(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/deploys")
	assertNavActive(t, body, "Deploys")
	if !strings.Contains(body, `data-scope=""`) {
		t.Fatal("/deploys must render the global shell, not a workspace")
	}
	if !strings.Contains(body, `href="/deploys" data-icon="deploys" class="active">Deploys</a>`) {
		t.Fatalf("global nav anchor does not follow the icon-then-class convention:\n%s", body)
	}
	// The workspace Deploys page keeps its own scoped anchor active — the two
	// nav modes share the IsDeploys flag and must not both render.
	ws := getBody(t, srv.Handler(), "/projects/proj/deploys")
	if strings.Contains(ws, `href="/deploys"`) {
		t.Fatal("workspace shell rendered the global Deploys link")
	}
}

// TestDeploysBoardPartialHasNoLayoutChrome pins the SSE fragment contract: the
// live region swaps innerHTML, so a fragment carrying layout would nest a whole
// page inside the board every two seconds.
func TestDeploysBoardPartialHasNoLayoutChrome(t *testing.T) {
	srv, _, _ := testServer(t)
	seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "frag123", ShortSHA: "frag123", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
	})
	req := httptest.NewRequest(http.MethodGet, "/partials/deploys/board", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	partial := w.Body.String()
	if !strings.Contains(partial, "frag123") {
		t.Fatalf("partial missing its rows:\n%s", partial)
	}
	for _, chrome := range []string{"<nav", "sse-status", "htmx.min.js", `id="live-root"`, `id="page-deploys-board"`} {
		if strings.Contains(partial, chrome) {
			t.Fatalf("partial leaked layout chrome %q", chrome)
		}
	}
}

// TestDeploysBoardSaysWhenItClipped pins that a bounded fold admits it. A lane
// missing because the scan stopped early would otherwise read as "this
// environment does not exist", which is a lie the board must not tell.
func TestDeploysBoardSaysWhenItClipped(t *testing.T) {
	srv, _, _ := testServer(t)
	srv.deployScanLimit = 2
	for _, svc := range []string{"one", "two", "three", "four"} {
		seedDeployRun(t, srv, deploy.Run{
			Project: "proj", Service: svc, Env: "prod",
			SHA: svc, ShortSHA: svc, Status: deploy.StatusSucceeded,
			QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
		})
	}
	body := getBody(t, srv.Handler(), "/deploys")
	if !strings.Contains(body, "2 most recently updated deploy runs") {
		t.Fatalf("clipped board did not say so:\n%s", body)
	}

	// Unclipped: the notice is noise on a board that is complete.
	srv2, _, _ := testServer(t)
	seedDeployRun(t, srv2, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "abcdef1", ShortSHA: "abcdef1", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-07-01T00:00:00Z", EndedAt: "2026-07-01T00:05:00Z",
	})
	full := getBody(t, srv2.Handler(), "/deploys")
	if strings.Contains(full, "most recently updated deploy runs") {
		t.Fatal("complete board rendered the truncation notice")
	}
	if !strings.Contains(full, "abcdef1") {
		t.Fatal("complete board lost its only lane")
	}
}

// TestDeploysBoardNeverClaimsNeverDeployedOnAClippedScan is the difference
// between "nothing ever shipped here" and "this lane's success is older than
// the scan window". LaneStates reports both as HasCurrent false, and the board
// must not turn the second one into a statement about production.
//
// The shape is the realistic one: nothing prunes the run store, a lane last
// succeeded weeks ago (its record untouched since — markSuperseded never ran on
// it), other lanes have been busy since, and one recent attempt on this lane is
// inside the window. Reading "never deployed" for a live production lane, in
// red, in the attention count, is worse than reading nothing.
func TestDeploysBoardNeverClaimsNeverDeployedOnAClippedScan(t *testing.T) {
	srv, _, _ := testServer(t)
	srv.deployScanLimit = 1

	old := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "0ldl1ve", ShortSHA: "0ldl1ve", Status: deploy.StatusSucceeded,
		QueuedAt: "2026-06-01T00:00:00Z", EndedAt: "2026-06-01T00:05:00Z",
		ActorName: "Dana Ops",
	})
	inFlight := seedDeployRun(t, srv, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "n3wshaa", ShortSHA: "n3wshaa", Status: deploy.StatusRunning,
		QueuedAt: "2026-07-20T00:00:00Z",
		ActorName: "Dana Ops",
	})
	// Pin the scan order: the six-week-old success falls outside the window,
	// the recent attempt does not. Filesystem timestamp resolution must not get
	// a vote.
	touchDeployRun(t, srv, old.ID, time.Now().Add(-42*24*time.Hour))
	touchDeployRun(t, srv, inFlight.ID, time.Now())

	body := getBody(t, srv.Handler(), "/deploys")
	if !strings.Contains(body, "2 most recently updated deploy runs") &&
		!strings.Contains(body, "1 most recently updated deploy runs") {
		t.Fatalf("clipped board did not say so:\n%s", body)
	}
	if strings.Contains(body, "never deployed") || strings.Contains(body, ">no deploy<") {
		t.Fatalf("clipped board asserted a lane never shipped:\n%s", body)
	}
	if !strings.Contains(body, "no successful deploy in the scanned window") {
		t.Fatalf("clipped lane did not say what it actually knows:\n%s", body)
	}
	// Not attention-worthy either: the newest run is merely in flight, and the
	// missing success is unproven, so the counter must stay at zero.
	if !strings.Contains(body, "all settled") {
		t.Fatalf("a clipped-out success was counted as an unshipped lane:\n%s", body)
	}

	// The honest claim survives on a board that read everything: here the lane
	// really has never succeeded, and the board says so in red.
	srv2, _, _ := testServer(t)
	seedDeployRun(t, srv2, deploy.Run{
		Project: "proj", Service: "api", Env: "prod",
		SHA: "fa1led0", ShortSHA: "fa1led0", Status: deploy.StatusFailed,
		QueuedAt: "2026-07-20T00:00:00Z", EndedAt: "2026-07-20T00:01:00Z",
	})
	full := getBody(t, srv2.Handler(), "/deploys")
	if !strings.Contains(full, "never deployed") {
		t.Fatalf("an unclipped board must still name a lane that never shipped:\n%s", full)
	}
	if strings.Contains(full, "no deploy in window") {
		t.Fatalf("complete board hedged a fact it had read:\n%s", full)
	}
}

// TestDeploysBoardEmptyState keeps the page useful before anything has ever
// been deployed: the nav item is reachable from day one.
func TestDeploysBoardEmptyState(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/deploys")
	if !strings.Contains(body, "No deploys recorded yet") {
		t.Fatalf("empty board missing its empty state:\n%s", body)
	}
}
