package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
)

func TestStartFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions false
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"do the thing"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStartViewerForbidden(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, csrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"do the thing"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestStartBadCSRF(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, "wrong-csrf", url.Values{
		"prompt": {"do the thing"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

// The "no sessions yet" empty states offer a start link, and unlike the always-
// present sidebar entry they are the only thing on the screen — so they follow
// the local convention (project_overview's own New task button) and gate on
// CanStartSession rather than sending a viewer somewhere that can only say no.
func TestSessionsEmptyStateStartLinkGated(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	const (
		empty  = "No sessions in this project yet."
		withLn = empty + ` <a href="/projects/proj/start">Start one</a>.`
	)
	for _, path := range []string{"/projects/proj", "/projects/proj/sessions"} {
		for _, tc := range []struct {
			role config.WebRole
			id   string
			want string
		}{
			{config.WebRoleMember, "member-1", withLn},
			{config.WebRoleViewer, "viewer-1", empty + "</div>"},
		} {
			sid, _, err := srv.LoginAs(tc.id, "U", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("%s as %s status=%d", path, tc.role, w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("%s as %s: empty state missing %q", path, tc.role, tc.want)
			}
		}
	}
}

func TestStartCreatesSessionRedirect(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"title":  {"Ship the widget"},
		"prompt": {"add a widget and open a PR"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/th-web-1") {
		t.Fatalf("Location=%q want /sessions/th-web-1", loc)
	}
	if !strings.Contains(loc, "ok=Session+started") {
		t.Fatalf("Location=%q want ok=Session+started", loc)
	}
	// Owner + web origin stamped so the creator can cancel/reset their own unit.
	e, ok := srv.sessions.Get("th-web-1")
	if !ok {
		t.Fatal("session th-web-1 not created")
	}
	if e.Origin != "web" {
		t.Fatalf("origin=%q want web", e.Origin)
	}
	if e.CreatedBy != "member-1" {
		t.Fatalf("createdBy=%q want member-1", e.CreatedBy)
	}
	if e.OwnerID != "member-1" {
		t.Fatalf("ownerID=%q want member-1", e.OwnerID)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
}

func TestStartWebNativeFallback(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, nil) // no API + Discord not ready → web-native unit
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"investigate the flake"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/w_") {
		t.Fatalf("Location=%q want web-native /sessions/w_*", loc)
	}
}

func TestStartEmptyPromptRedirectsBack(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	spy := &bot.FakeThreadAPI{NextTh: "should-not"}
	bot.SetThreadAPIForTest(b, spy)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"   "},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/start") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want start page with err", loc)
	}
	if spy.StartCount() != 0 {
		t.Fatalf("must not create a unit on empty prompt (created %d)", spy.StartCount())
	}
}

func TestStartInvestigateModeAudited(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"look into the timeout"},
		"mode":   {"investigate"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/th-web-1") {
		t.Fatalf("Location=%q", loc)
	}
	assertAuditDetailContains(t, srv, `"origin":"web-start"`)
	assertAuditDetailContains(t, srv, `"mode":"investigate"`)
}

func TestStartCrossProjectPostForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	srv.cfg.WebAuth.Features.StartSessions = true
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// member-1 is not on the secret project allowlist.
	w := postFix(t, srv, "/projects/secret/start", sid, csrf, url.Values{
		"prompt": {"peek at secret"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStartPageShowsFormForMember(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/start", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-start"`,
		`id="btn-start"`,
		`<form class="stack" method="post" action="/projects/proj/start">`,
		`name="prompt"`,
		`name="title"`,
		`name="mode"`,
		`value="investigate"`,
		`value="explain"`,
		// proj default is fix → the empty option is the fix label and the ship copy
		// reads "When a run ships:".
		`Fix &amp; ship (default)`,
		`When a run ships:`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("start page missing %q", want)
		}
	}
	// Fix-default project: no separate explicit fix option, no non-fix ship caveat.
	if strings.Contains(body, `value="fix"`) {
		t.Fatal("fix-default project must not render a separate value=\"fix\" option")
	}
	if strings.Contains(body, "Project default mode is") {
		t.Fatal("fix-default project must not render the non-fix ship caveat")
	}
	assertNavActive(t, body, "Start task")
}

// A project whose default mode is non-fix must render the mode select and the
// "What happens" copy honestly: the empty option is the project default, an
// explicit fix option is offered, and the ship copy warns investigate/explain
// never ship.
func TestStartPageNonFixDefaultMode(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	if err := cfg.SetProjectSafeTeam("proj", false, "", "investigate"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/start", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`Project default (investigate)`,
		`<option value="fix">Fix &amp; ship</option>`,
		`Project default mode is`,
		`never opens PRs or ships`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("non-fix start page missing %q", want)
		}
	}
	if strings.Contains(body, `Fix &amp; ship (default)`) {
		t.Fatal("non-fix default must not label the empty option as the fix default")
	}
}

func TestStartPageReadOnlyForViewer(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/start", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="btn-start"`) {
		t.Fatal("viewer must not see the start form")
	}
	if !strings.Contains(body, "Read-only access") {
		t.Fatalf("viewer missing read-only fallback: %s", body[:min(500, len(body))])
	}
}

// Investigator (Safe Team) must not see Fix & ship and cannot POST it.
func TestStartInvestigatorHidesFixAndBlocksPOST(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	// Map member as investigator explicitly (Safe Team default is investigator too).
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// GET: no Fix & ship option
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/start", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Option labels (not the "Fix & ship requires …" rail note).
	if strings.Contains(body, `value="fix"`) || strings.Contains(body, `Fix &amp; ship (default)`) {
		t.Fatalf("investigator must not see Fix & ship mode option: %s", body[:min(1200, len(body))])
	}
	if !strings.Contains(body, `value="investigate"`) {
		t.Fatal("investigate option required")
	}
	if !strings.Contains(body, "builder-class") {
		t.Fatal("expected rail note about builder-class caps")
	}
	// POST mode=fix → redirect with err
	w2 := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"ship it"},
		"mode":   {"fix"},
	})
	if w2.Code != http.StatusFound && w2.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
	loc := w2.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/start") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want start page with err", loc)
	}
	if !strings.Contains(loc, "startSessions") && !strings.Contains(loc, "not+allowed") && !strings.Contains(loc, "not%20allowed") {
		// err is URL-encoded; accept either encoding of the sentinel message
		if !strings.Contains(loc, "githubWrites") && !strings.Contains(strings.ToLower(loc), "fix") {
			t.Fatalf("Location=%q want fix-denied err", loc)
		}
	}
	// POST investigate still works
	w3 := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"look into flake"},
		"mode":   {"investigate"},
	})
	if w3.Code != http.StatusFound && w3.Code != http.StatusSeeOther {
		t.Fatalf("investigate status=%d body=%s", w3.Code, w3.Body.String())
	}
	if loc3 := w3.Header().Get("Location"); !strings.HasPrefix(loc3, "/sessions/") {
		t.Fatalf("investigate Location=%q", loc3)
	}
}

// The model picker is builder-class only, and the gate is on the POST too — a page
// that hides the field is not a permission check.
func TestStartModelPickerBuilderClassOnly(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/projects/proj/start")
	if strings.Contains(body, `name="model"`) {
		t.Fatal("investigator must not see the model field")
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"look into this"},
		"mode":   {"investigate"},
		"model":  {"claude-opus-5"},
	})
	// Assert the *reason*, not just that something failed — "err= is present" passes
	// for any unrelated breakage on this POST.
	assertRedirectErr(t, w, "/projects/proj/start", "not allowed to pick a model")
	// And nothing was started on the requested model.
	for _, l := range srv.sessions.List() {
		if l.Entry.Model != "" {
			t.Fatalf("session %s stamped model %q despite denial", l.ThreadID, l.Entry.Model)
		}
	}
}

// assertRedirectErr asserts a redirect back to path carrying err= containing want.
func assertRedirectErr(t *testing.T, w *httptest.ResponseRecorder, path, want string) {
	t.Helper()
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, path) {
		t.Fatalf("Location=%q want prefix %q", loc, path)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location=%q: %v", loc, err)
	}
	got := u.Query().Get("err")
	if !strings.Contains(got, want) {
		t.Fatalf("err=%q want it to contain %q", got, want)
	}
}

// A builder's pick is stamped on the session, and the agent follows the model —
// the two cannot disagree, since a session id is not portable between CLIs.
func TestStartModelPickStampsSession(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/projects/proj/start")
	if !strings.Contains(body, `id="start-model"`) || !strings.Contains(body, `value="claude-opus-5"`) {
		t.Fatal("builder must see the model field with curated options")
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"ship it"},
		"model":  {"claude-opus-5"},
	})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("status=%d Location=%q", w.Code, loc)
	}
	tid := strings.TrimPrefix(loc, "/sessions/")
	if i := strings.IndexAny(tid, "?&"); i >= 0 {
		tid = tid[:i]
	}
	e, ok := srv.sessions.Get(tid)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5" {
		t.Fatalf("want claude stamped, got ok=%v agent=%q model=%q", ok, e.Agent, e.Model)
	}
}

// An unknown name is rejected outright rather than handed to a CLI that has never
// heard of it.
func TestStartModelPickRejectsUnknownName(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/start", sid, csrf, url.Values{
		"prompt": {"ship it"},
		"model":  {"gpt-9"},
	})
	// The message must name the rejected model, so an operator can tell a typo from
	// a permissions problem.
	assertRedirectErr(t, w, "/projects/proj/start", "gpt-9")
}

func getPageBody(t *testing.T, srv *Server, sid, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// assertAuditDetailContains asserts today's audit log contains substr somewhere.
func assertAuditDetailContains(t *testing.T, srv *Server, substr string) {
	t.Helper()
	if srv.audit == nil {
		t.Fatal("no audit")
	}
	entries, err := os.ReadDir(srv.audit.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(srv.audit.Dir(), ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), substr) {
			return
		}
	}
	t.Fatalf("audit detail %q not found", substr)
}
