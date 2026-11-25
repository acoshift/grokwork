package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestProjectDisabledSetting(t *testing.T) {
	srv, cfg, _ := testServer(t)
	other := t.TempDir()
	cfg.Projects["other"] = config.ProjectConfig{Path: other}
	h := srv.Handler()

	body := getConfig(t, h, "/config/projects/proj/integrations")
	if !strings.Contains(body, `id="project-disabled"`) {
		t.Fatal("integrations page missing the disable control")
	}
	if !strings.Contains(body, "Background jobs are on for this project") {
		t.Fatal("page did not show background jobs on")
	}
	if strings.Contains(body, `name="disabled" value="1" checked`) {
		t.Fatal("control rendered checked while the project is active")
	}

	postDisabled := func(h http.Handler, disabled string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"name": {"proj"}}
		if disabled != "" {
			form.Set("disabled", disabled)
		}
		req := httptest.NewRequest(http.MethodPost, "/config/projects/disabled", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	w := postDisabled(h, "1")
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("disable status=%d body=%s", w.Code, w.Body.String())
	}
	if !cfg.ProjectDisabled("proj") {
		t.Fatal("live config is not disabled after POST")
	}
	if cfg.ProjectDisabled("other") {
		t.Fatal("disabling proj also disabled other")
	}
	if !projectSnapshotDisabled(cfg, "proj") || projectSnapshotDisabled(cfg, "other") {
		t.Fatal("snapshot did not report the flag")
	}
	body = getConfig(t, h, "/config/projects/proj/integrations")
	if !strings.Contains(body, "Background jobs are off for this project") {
		t.Fatal("page did not show background jobs off")
	}
	if !strings.Contains(body, `name="disabled" value="1" checked`) {
		t.Fatal("checkbox was not checked")
	}

	w = postDisabled(h, "")
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("clear status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg.ProjectDisabled("proj") {
		t.Fatal("live config still disabled after clearing POST")
	}
	if projectSnapshotDisabled(cfg, "proj") {
		t.Fatal("snapshot still reports disabled")
	}
	body = getConfig(t, h, "/config/projects/proj/integrations")
	if !strings.Contains(body, "Background jobs are on for this project") {
		t.Fatal("page did not show background jobs on after clear")
	}
	if strings.Contains(body, `name="disabled" value="1" checked`) {
		t.Fatal("checkbox stayed checked after clear")
	}

	authSrv, authCfg, _ := authOnServer(t)
	sid, csrf, err := authSrv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"proj"}, "disabled": {"1"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/config/projects/disabled", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	authSrv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member POST status=%d body=%s", w.Code, w.Body.String())
	}
	if authCfg.ProjectDisabled("proj") {
		t.Fatal("non-admin POST persisted a disable")
	}
}

func getConfig(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func projectSnapshotDisabled(cfg *config.Config, name string) bool {
	for _, item := range cfg.Snapshot().Projects {
		if item.Name == name {
			return item.Disabled
		}
	}
	return false
}
