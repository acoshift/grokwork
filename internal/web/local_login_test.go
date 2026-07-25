package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// enableLocalAuth provisions one non-Discord account and turns web auth on.
func enableLocalAuth(t *testing.T, srv *Server, cfg *config.Config, password string) {
	t.Helper()
	hash, err := config.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WebAuth = &config.WebAuthConfig{
		Enabled: true,
		LocalAccounts: []config.LocalAccount{
			{ID: "alice", DisplayName: "Alice", PasswordHash: hash, Role: config.WebRoleMember},
		},
	}
}

func postLocal(t *testing.T, srv *Server, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/auth/local", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestLocalLoginCreatesSession is the goal of the whole train: someone with no
// Discord account can sign in. Every other identity path starts from a snowflake.
func TestLocalLoginCreatesSession(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableLocalAuth(t, srv, cfg, "a-very-good-password")

	w := postLocal(t, srv, "alice", "a-very-good-password")
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status = %d, want a redirect after login", w.Code)
	}
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie issued")
	}
	sess, _, ok := srv.webSessions.Get(session)
	if !ok {
		t.Fatal("session not stored")
	}
	if sess.DiscordUserID != "local:alice" {
		t.Errorf("actor id = %q, want the namespaced local id", sess.DiscordUserID)
	}
	if sess.Role != config.WebRoleMember {
		t.Errorf("role = %q, want member", sess.Role)
	}
}

func TestLocalLoginRejectsBadPassword(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableLocalAuth(t, srv, cfg, "a-very-good-password")

	w := postLocal(t, srv, "alice", "not-the-password")
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatal("a failed login issued a session cookie")
		}
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/login") {
		t.Errorf("Location = %q, want a bounce back to /login", loc)
	}
}

// TestLocalLoginDisabledWhenAuthOff: with web auth off the UI is open LAN mode, so
// a password form would imply a protection that does not exist.
func TestLocalLoginDisabledWhenAuthOff(t *testing.T) {
	srv, cfg, _ := testServer(t)
	hash, err := config.HashPassword("a-very-good-password")
	if err != nil {
		t.Fatal(err)
	}
	// Accounts provisioned, but auth disabled.
	cfg.WebAuth = &config.WebAuthConfig{
		Enabled:       false,
		LocalAccounts: []config.LocalAccount{{ID: "alice", PasswordHash: hash}},
	}
	if w := postLocal(t, srv, "alice", "a-very-good-password"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 while auth is off", w.Code)
	}
}

// TestLoginPageShowsPasswordFormOnlyWhenProvisioned avoids inviting guesses at
// credentials that cannot exist.
func TestLoginPageShowsPasswordFormOnlyWhenProvisioned(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Body.String()
	}
	if body := get(); strings.Contains(body, `id="login-local"`) {
		t.Error("password form rendered with no accounts provisioned")
	}

	enableLocalAuth(t, srv, cfg, "a-very-good-password")
	body := get()
	if !strings.Contains(body, `id="login-local"`) {
		t.Error("password form missing despite a provisioned account")
	}
	if !strings.Contains(body, `id="login-discord"`) {
		t.Error("Discord login must remain available alongside local login")
	}
}

// TestInboxPageShowsQueuedNotifications closes the loop for a non-Discord member:
// they can log in (local account), a finished run they watched cannot reach them
// by DM, and the notification is readable here rather than dropped.
func TestInboxPageShowsQueuedNotifications(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableLocalAuth(t, srv, cfg, "a-very-good-password")

	store := srv.bot.Inbox()
	if store == nil {
		t.Fatal("inbox store not initialized on the test bot")
	}
	if err := srv.bot.QueueInbox("local:alice", "run.done",
		"Run done · 2s · proj", "fix the flaky test", "/sessions/w_1", "w_1", "proj"); err != nil {
		t.Fatal(err)
	}

	sessionID, _, err := srv.LoginAs("local:alice", "Alice", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-inbox"`) {
		t.Error("missing page marker")
	}
	for _, want := range []string{"Run done · 2s · proj", "fix the flaky test", "/sessions/w_1"} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox page missing %q", want)
		}
	}
}

func TestInboxPageEmptyState(t *testing.T) {
	srv, cfg, _ := testServer(t)
	enableLocalAuth(t, srv, cfg, "a-very-good-password")
	sessionID, _, err := srv.LoginAs("local:bob", "Bob", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "Nothing queued") {
		t.Error("empty state missing")
	}
}
