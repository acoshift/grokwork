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

func TestConfigMutateWritesAuditAuthOff(t *testing.T) {
	srv, cfg, _ := testServer(t)
	form := url.Values{"section": {"worktree"}, "worktreeIdleTTLDays": {"14"}}
	req := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg.WorktreeIdleTTLDaysValue() != 14 {
		t.Fatalf("ttl=%d", cfg.WorktreeIdleTTLDaysValue())
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("expected audit event")
	}
	last := evs[len(evs)-1]
	if last.Action != audit.ActionConfigSettings {
		t.Fatalf("action=%q", last.Action)
	}
	if last.Actor != audit.ActorAnonymous {
		t.Fatalf("actor=%q want anonymous (auth off)", last.Actor)
	}
	if !last.OK || last.Time.IsZero() {
		t.Fatalf("event=%+v", last)
	}
	if last.Detail["section"] != "worktree" {
		t.Fatalf("detail=%v", last.Detail)
	}
}

func TestConfigMutateWritesAuditAuthOn(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"name": {"audit-proj"},
		"path": {cfg.DataDir}, // absolute path under data dir works as project path for AddProject
		"csrf": {csrf},
	}
	// AddProject needs an existing directory — DataDir exists.
	req := httptest.NewRequest(http.MethodPost, "/config/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := cfg.ProjectPath("audit-proj"); !ok {
		t.Fatal("project not added")
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range evs {
		if ev.Action == audit.ActionConfigAddProject && ev.Actor == "admin-1" && ev.OK {
			found = true
			if ev.Role != string(config.WebRoleAdmin) {
				t.Fatalf("role=%q", ev.Role)
			}
		}
	}
	if !found {
		t.Fatalf("missing add_project audit in %+v", evs)
	}
}

func TestLoginFailWritesAudit(t *testing.T) {
	srv, _, _ := authOnServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/auth/discord", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	state := strings.SplitN(stateCookie.Value, "|", 2)[0]
	req = httptest.NewRequest(http.MethodGet, "/auth/discord/callback?code=code-deny&state="+state, nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Action == audit.ActionLoginFail && ev.Actor == "stranger" && !ev.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing login.fail: %+v", evs)
	}
}
