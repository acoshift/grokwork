package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/inbox"
)

// TestInboxPageShowsQueuedNotifications closes the loop for a non-Discord member:
// they can log in (local account), a finished run they watched cannot reach them
// by DM, and the notification is readable here rather than dropped.
func TestInboxPageShowsQueuedNotifications(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}

	pc := cfg.Projects["proj"]
	pc.AllowedUserIDs = append(pc.AllowedUserIDs, "oidc:alice")
	cfg.Projects["proj"] = pc
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
	body := w.Body.String()
	if !strings.Contains(body, "Nothing queued") {
		t.Error("empty state missing")
	}
	// An empty feed must still mount the live region so the first item
	// arriving over sse:inbox paints without a full navigation.
	if !strings.Contains(body, `id="live-inbox"`) {
		t.Fatal("empty inbox missing live-inbox region")
	}
	if !strings.Contains(body, `hx-trigger="sse:inbox"`) {
		t.Fatal("empty inbox missing sse:inbox trigger")
	}
}

func TestInboxGETDoesNotMarkRead(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}
	if err := srv.bot.QueueInbox("oidc:alice", inbox.KindRunDone, "hello", "", "/sessions/w_1", "w_1", ""); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("oidc:alice", "Alice", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if n := srv.bot.Inbox().UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("GET marked read, unread=%d", n)
	}
	if !strings.Contains(w.Body.String(), "unread") {
		t.Error("unread chip missing")
	}
}

func TestInboxMarkAllReadAndCountsJSON(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{
		Enabled: true, SessionSecret: "test-session-secret-32-bytes-long!",
	}
	store := srv.bot.Inbox()
	for _, s := range []string{"a", "b", "c"} {
		if _, err := store.Append("oidc:alice", inbox.Item{Kind: inbox.KindRunDone, Subject: s}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkRead("oidc:alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRead("oidc:alice", 2); err != nil {
		t.Fatal(err)
	}
	if n := store.UnreadCount("oidc:alice"); n != 1 {
		t.Fatalf("setup unread=%d", n)
	}
	sid, csrf, err := srv.LoginAs("oidc:alice", "Alice", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"all": {"1"}, "csrf": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/inbox/read", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if n := store.UnreadCount("oidc:alice"); n != 0 {
		t.Fatalf("unread after mark-all=%d", n)
	}
	code, got, body := getNavCounts(t, srv, "/partials/nav/counts", &http.Cookie{Name: sessionCookieName, Value: sid})
	if code != http.StatusOK {
		t.Fatalf("counts status=%d body=%s", code, body)
	}
	if _, ok := got["inbox"]; !ok {
		t.Fatalf("inbox key missing from %s", body)
	}
	if got["inbox"] != 0 {
		t.Fatalf("inbox=%d want 0 (key present): %s", got["inbox"], body)
	}
	if !strings.Contains(body, `"inbox":0`) && !strings.Contains(body, `"inbox": 0`) {
		t.Fatalf("JSON must include inbox even at zero: %s", body)
	}
}

func TestInboxOmitsForbiddenProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.bot.QueueInbox("member-1", inbox.KindRunDone, "visible", "", "/sessions/a", "a", "public"); err != nil {
		t.Fatal(err)
	}
	if err := srv.bot.QueueInbox("member-1", inbox.KindRunDone, "secret-goal", "", "/sessions/b", "b", "secret"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "visible") {
		t.Error("missing visible item")
	}
	if strings.Contains(body, "secret-goal") {
		t.Error("forbidden project leaked into the feed")
	}
	code, got, countsBody := getNavCounts(t, srv, "/partials/nav/counts", cookie)
	if code != http.StatusOK {
		t.Fatalf("counts=%d %s", code, countsBody)
	}
	if got["inbox"] != 1 {
		t.Fatalf("unread visible=%d want 1: %v", got["inbox"], got)
	}
}

func TestInboxBellHiddenAuthOff(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/")
	if strings.Contains(body, `class="inbox-bell`) {
		t.Fatal("auth-off layout must not render the bell")
	}
	if !strings.Contains(body, `data-nav-count="inbox"`) {
		t.Fatal("global Inbox nav still needs the count placeholder")
	}
	assertNavActive(t, getBody(t, srv.Handler(), "/inbox"), "Inbox")
}

// Desktop parks the bell in the profile grid (avatar | identity | inbox);
// the phone bar keeps it as a sibling of the avatar, not inside the pop.
func TestInboxBellLivesInProfile(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()

	userStart := strings.Index(body, `<div class="side-user">`)
	if userStart < 0 {
		t.Fatal("profile section missing")
	}
	popStart := strings.Index(body[userStart:], `<div class="side-user-pop" id="user-menu">`)
	if popStart < 0 {
		t.Fatal("account pop missing")
	}
	beforePop := body[userStart : userStart+popStart]
	if !strings.Contains(beforePop, `class="inbox-bell`) {
		t.Fatal("inbox bell must sit in the profile, outside the account pop")
	}
	popEndRel := strings.Index(body[userStart+popStart:], "</div>")
	if popEndRel < 0 {
		t.Fatal("account pop not closed")
	}
	pop := body[userStart+popStart : userStart+popStart+popEndRel]
	if strings.Contains(pop, `class="inbox-bell`) {
		t.Fatal("inbox bell inside the account pop would vanish from the phone top bar")
	}

	footStart := strings.Index(body, `<div class="side-foot">`)
	if footStart < 0 || footStart > userStart {
		t.Fatal("profile must live inside the footer")
	}
	between := body[footStart:userStart]
	if strings.Contains(between, `class="inbox-bell`) {
		t.Fatal("inbox bell still sits above the profile instead of in it")
	}

	nav := navLinksChunk(t, body)
	if strings.Contains(nav, `data-icon="inbox"`) {
		t.Fatal("signed-in desktop nav must not duplicate Inbox; the profile bell is the entry")
	}
}

func TestFpInboxIsolatedPerActor(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.bot.QueueInbox("member-1", inbox.KindRunDone, "m", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	sidA, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	sidB, _, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	reqA := httptest.NewRequest(http.MethodGet, "/events", nil)
	reqA.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sidA})
	reqB := httptest.NewRequest(http.MethodGet, "/events", nil)
	reqB.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sidB})
	a := srv.fpInbox(reqA)
	b := srv.fpInbox(reqB)
	if a == "" || b == "" {
		t.Fatalf("empty fp a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatal("two actors with different feeds must not share an inbox rev")
	}
}

func TestInboxPartialHasNoChrome(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true}
	sid, _, err := srv.LoginAs("oidc:alice", "Alice", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/partials/inbox/list", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, bad := range []string{"<nav", "sse-status", "htmx.min.js"} {
		if strings.Contains(body, bad) {
			t.Errorf("partial contains chrome %q", bad)
		}
	}
}
