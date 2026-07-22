package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
)

func TestCaseNewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions false
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title": {"Checkout 500s"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCaseNewViewerForbidden(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, csrf, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title": {"Checkout 500s"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCaseNewBadCSRF(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, "wrong-csrf", url.Values{
		"title": {"Checkout 500s"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCaseNewCrossProjectForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	srv.cfg.WebAuth.Features.StartSessions = true
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// member-1 is not on the secret project allowlist.
	w := postFix(t, srv, "/projects/secret/cases/new", sid, csrf, url.Values{
		"title": {"peek"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCaseCreateIntakeShell pins the core contract: POST creates a case shell
// (Mode=case, Phase=intake, severity/ref/reporter/intake-source stamped), does
// NOT run Grok, and redirects to the session workspace with a flash.
func TestCaseCreateIntakeShell(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	spy := &bot.FakeThreadAPI{NextMsg: "m1", NextTh: "th-web-1"}
	bot.SetThreadAPIForTest(b, spy)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title":    {"Checkout 500s for EU Visa cards"},
		"severity": {"critical"},
		"ref":      {"ZD-4821"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/th-web-1") {
		t.Fatalf("Location=%q want /sessions/th-web-1", loc)
	}
	if !strings.Contains(loc, "ok=case+opened") {
		t.Fatalf("Location=%q want ok=case+opened flash", loc)
	}
	if !strings.Contains(loc, "project=proj") {
		t.Fatalf("Location=%q want project=proj scope", loc)
	}
	e, ok := srv.sessions.Get("th-web-1")
	if !ok {
		t.Fatal("session th-web-1 not created")
	}
	if e.Mode != "case" {
		t.Fatalf("mode=%q want case", e.Mode)
	}
	if e.Phase != "intake" {
		t.Fatalf("phase=%q want intake", e.Phase)
	}
	if e.Severity != "critical" {
		t.Fatalf("severity=%q want critical", e.Severity)
	}
	if e.CustomerTitle != "Checkout 500s for EU Visa cards" {
		t.Fatalf("customerTitle=%q", e.CustomerTitle)
	}
	if e.CustomerRef != "ZD-4821" {
		t.Fatalf("customerRef=%q", e.CustomerRef)
	}
	if e.ReporterID != "member-1" {
		t.Fatalf("reporterID=%q want member-1", e.ReporterID)
	}
	if e.IntakeSource != "web" {
		t.Fatalf("intakeSource=%q want web", e.IntakeSource)
	}
	if e.Origin != "web" {
		t.Fatalf("origin=%q want web", e.Origin)
	}
	if e.Goal != "Checkout 500s for EU Visa cards" {
		t.Fatalf("goal=%q want title", e.Goal)
	}
	if e.Label != "open" {
		t.Fatalf("label=%q want open", e.Label)
	}
	if e.OwnerID != "member-1" {
		t.Fatalf("ownerID=%q want member-1", e.OwnerID)
	}
	// Intake-only: no Grok run (Discord /case parity).
	bot.WaitIdleForTest(b, 5*time.Second)
	if e, _ := srv.sessions.Get("th-web-1"); e.SessionID != "" {
		t.Fatalf("intake-only case must not run Grok (sessionID=%q)", e.SessionID)
	}
	// The Discord starter carries the same case card as Discord "/case".
	if len(spy.Sends) != 1 || !strings.Contains(spy.Sends[0], "**Case**") ||
		!strings.Contains(spy.Sends[0], "Checkout 500s for EU Visa cards") {
		t.Fatalf("starter message missing case card: %v", spy.Sends)
	}
	assertAuditDetailContains(t, srv, `"origin":"web-case"`)
}

// Notes queue an investigate run: phase promotes intake → investigate before
// the snapshot (K19) and Mode stays case.
func TestCaseCreateNotesQueuesInvestigate(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title":    {"Webhook retries duplicated"},
		"severity": {"high"},
		"notes":    {"Retries fire twice for one order; see staging burst at 09:14"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/th-web-1") || !strings.Contains(loc, "ok=case+opened") {
		t.Fatalf("Location=%q", loc)
	}
	e, ok := srv.sessions.Get("th-web-1")
	if !ok {
		t.Fatal("session th-web-1 not created")
	}
	if e.Mode != "case" {
		t.Fatalf("mode=%q want case (never fix/investigate)", e.Mode)
	}
	if e.Phase != "investigate" {
		t.Fatalf("phase=%q want investigate (promoted before run)", e.Phase)
	}
	assertAuditDetailContains(t, srv, `"investigate":true`)
}

func TestCaseCreateEmptyTitleRedirectsBack(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	spy := &bot.FakeThreadAPI{NextTh: "should-not"}
	bot.SetThreadAPIForTest(b, spy)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title":    {"   "},
		"severity": {"high"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/cases/new") || !strings.Contains(loc, "err=") {
		t.Fatalf("Location=%q want intake page with err", loc)
	}
	if spy.StartCount() != 0 {
		t.Fatalf("must not create a unit on empty title (created %d)", spy.StartCount())
	}
}

// Investigators (safe-team default template) must open cases without
// GithubWrites or StartSessions capability — the Discord /case gate.
func TestCaseCreateInvestigatorCapability(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	caps := cfg.ResolveCapabilities("proj", "member-1", nil)
	if caps.GithubWrites || caps.StartSessions {
		t.Fatalf("test setup: investigator must not have builder caps: %+v", caps)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title": {"Refund settles in wrong currency"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("investigator denied: status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/th-web-1") {
		t.Fatalf("Location=%q", loc)
	}
}

// A capability template with none of investigate/fileEscalation/startSessions
// cannot open cases even with a member web role.
func TestCaseCreateCapabilityDenied(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	pc := cfg.Projects["proj"]
	pc.CapabilityTemplates = map[string]config.Capabilities{"support-view": {DraftCustomerReply: true}}
	pc.CapabilityByUser = map[string]string{"member-1": "support-view"}
	cfg.Projects["proj"] = pc
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title": {"Checkout 500s"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// No gateway and no thread API → web-native w_* unit, still a full case shell.
func TestCaseCreateWebNativeFallback(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bot.SetThreadAPIForTest(b, nil)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cases/new", sid, csrf, url.Values{
		"title": {"Rate limit header missing"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/w_") {
		t.Fatalf("Location=%q want web-native /sessions/w_*", loc)
	}
	threadID := strings.TrimPrefix(loc, "/sessions/")
	if i := strings.IndexByte(threadID, '?'); i >= 0 {
		threadID = threadID[:i]
	}
	e, ok := srv.sessions.Get(threadID)
	if !ok {
		t.Fatalf("session %s not created", threadID)
	}
	if e.Mode != "case" || e.Phase != "intake" {
		t.Fatalf("mode=%q phase=%q want case/intake", e.Mode, e.Phase)
	}
	if !strings.HasPrefix(e.WorktreeBranch, "grok/web/") {
		t.Fatalf("worktreeBranch=%q want grok/web/ prefix", e.WorktreeBranch)
	}
}

func TestCaseNewPageForm(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-case-new"`,
		`id="btn-case-new"`,
		`<form class="stack" method="post" action="/projects/proj/cases/new">`,
		`name="title"`,
		`name="severity"`,
		`value="medium" checked`,
		`value="critical"`,
		`value="low"`,
		`name="ref"`,
		`name="notes"`,
		// Intake contract copy: no run until investigate, never ships.
		`intake`,
		`never opens PRs or ships`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("case intake page missing %q", want)
		}
	}
	assertNavActive(t, body, "Cases")
}

func TestCaseNewPageReadOnlyForViewer(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="btn-case-new"`) {
		t.Fatal("viewer must not see the intake form")
	}
	if !strings.Contains(body, "Read-only access") {
		t.Fatalf("viewer missing read-only fallback: %s", body[:min(500, len(body))])
	}
}

// The board offers New case in the header and the empty state, and the empty
// copy mentions both web and Discord intake.
func TestCasesBoardNewCaseCTA(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/cases", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if got := strings.Count(body, `href="/projects/proj/cases/new"`); got != 2 {
		t.Fatalf("want New case CTA in header + empty state (2 links), got %d", got)
	}
	for _, want := range []string{
		`id="cases-empty"`,
		"Open one here on the web",
		"@Grok /case",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cases board missing %q", want)
		}
	}

	// Viewers see neither CTA (POST would 403).
	vsid, _, err := srv.LoginAs("viewer-1", "V", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/proj/cases", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: vsid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `href="/projects/proj/cases/new"`) {
		t.Fatal("viewer must not see the New case CTA")
	}
}
