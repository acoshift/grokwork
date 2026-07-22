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
	if !strings.Contains(loc, "ok=started") {
		t.Fatalf("Location=%q want ok=started", loc)
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
		`never ship`,
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
