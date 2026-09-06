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
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func seedInvestigateSession(t *testing.T, srv *Server, threadID, ownerID string) {
	t.Helper()
	if err := srv.sessions.Set(threadID, sessionstore.Entry{
		Project:   "proj",
		Mode:      bot.ModeInvestigate,
		OwnerID:   ownerID,
		OwnerName: "Owner",
		Origin:    "web",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInvestigateSessionOffersStartFix(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedInvestigateSession(t, srv, "inv-offer", "member-1")
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/inv-offer?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="btn-start-fix"`,
		`name="intent"`,
		`value="fix"`,
		`id="session-mode"`,
		"investigate",
		`placeholder="What should the agent look into?"`,
		"Continue stays read-only. Fix &amp; ship implements in this session.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestFixSessionHidesStartFix(t *testing.T) {
	srv, _, _ := fixEnabledServer(t)
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	if err := srv.sessions.Set("fix-th", sessionstore.Entry{
		Project: "proj", Mode: bot.ModeFix, OwnerID: "member-1", Origin: "web",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/fix-th?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="btn-start-fix"`) {
		t.Fatal("fix session must not offer Start fix")
	}
	if !strings.Contains(body, "Queues a follow-up on this session.") {
		t.Fatal("fix session keep generic continue hint")
	}
}

func TestInvestigateSessionHidesStartFixForInvestigator(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg.AddProjectAllowedUser("proj", "member-1")
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	seedInvestigateSession(t, srv, "inv-hide", "member-1")
	sid, _, err := srv.LoginAs("member-1", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/inv-hide?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="btn-start-fix"`) {
		t.Fatal("investigator must not see Fix & ship")
	}
	if !strings.Contains(body, "Queues a read-only investigate. Never opens PRs.") {
		t.Fatal("investigator investigate hint")
	}
}

func TestPostSessionStartFixPromotes(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	_ = srv.cfg.AddProjectAllowedUser("proj", "member-1")
	seedInvestigateSession(t, srv, "inv-fix", "member-1")
	var kinds []bot.Kind
	bot.SetStartTaskHookForTest(b, func(opts bot.StartTaskOpts) {
		kinds = append(kinds, opts.Kind)
	})
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/inv-fix/continue", sid, csrf, url.Values{
		"prompt": {"fix the nil deref"},
		"intent": {"fix"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("start fix failed: %s", loc)
	}
	if len(kinds) != 1 || kinds[0] != bot.KindStartFix {
		t.Fatalf("kinds=%v", kinds)
	}
	ent, ok := srv.sessions.Get("inv-fix")
	if !ok {
		t.Fatal("missing session")
	}
	if ent.Mode != bot.ModeFix {
		t.Fatalf("mode=%q", ent.Mode)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/inv-fix?project=proj", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, req)
	if page.Code != http.StatusOK {
		t.Fatalf("page status=%d", page.Code)
	}
	body := page.Body.String()
	if strings.Contains(body, `id="btn-start-fix"`) {
		t.Fatal("after promote, Fix & ship must hide")
	}
	if !strings.Contains(body, `id="session-mode"`) || !strings.Contains(body, ">fix<") {
		t.Fatal("work unit should badge Mode=fix")
	}
}

func TestPostSessionStartFixForbiddenInvestigator(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	_ = cfg.AddProjectAllowedUser("proj", "member-1")
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	seedInvestigateSession(t, srv, "inv-403", "member-1")
	sid, csrf, err := srv.LoginAs("member-1", "Inv", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/sessions/inv-403/continue", sid, csrf, url.Values{
		"prompt": {"fix it"},
		"intent": {"fix"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ent, _ := srv.sessions.Get("inv-403")
	if ent.Mode != bot.ModeInvestigate {
		t.Fatalf("denied start-fix must not promote: mode=%q", ent.Mode)
	}
}
