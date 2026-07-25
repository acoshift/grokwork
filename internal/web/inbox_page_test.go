package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// TestInboxPageShowsQueuedNotifications closes the loop for a non-Discord member:
// they can log in (local account), a finished run they watched cannot reach them
// by DM, and the notification is readable here rather than dropped.
func TestInboxPageShowsQueuedNotifications(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}

	store := srv.bot.Inbox()
	if store == nil {
		t.Fatal("inbox store not initialized on the test bot")
	}
	if err := srv.bot.QueueInbox("oidc:alice", "run.done",
		"Run done · 2s · proj", "fix the flaky test", "/sessions/w_1", "w_1", "proj"); err != nil {
		t.Fatal(err)
	}

	sessionID, _, err := srv.LoginAs("oidc:alice", "Alice", config.WebRoleMember)
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
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}
	sessionID, _, err := srv.LoginAs("oidc:bob", "Bob", config.WebRoleMember)
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
