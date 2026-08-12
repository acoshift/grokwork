package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestMintRefusedWhenWebAuthOff(t *testing.T) {
	srv, _, _ := testServer(t)
	form := url.Values{"label": {"x"}, "project": {"proj"}}
	req := httptest.NewRequest(http.MethodPost, "/config/api-tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("open-LAN mint status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "gw_") {
		t.Fatal("secret leaked on open-LAN mint")
	}
}

func TestMintAdminShowOnce(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"label": {"cloud-vm"}, "project": {"proj"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/config/api-tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gw_") {
		t.Fatalf("missing show-once secret: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token:") {
		t.Fatal("missing actor id")
	}
	if !strings.Contains(w.Body.String(), `id="page-config-api-tokens"`) {
		t.Fatal("missing page marker")
	}
}

func TestMintMemberForbidden(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"label": {"x"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/config/api-tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}
