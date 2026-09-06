package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestTodayPagesRenderAndNav(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	global := getBody(t, h, "/today")
	if !strings.Contains(global, `id="page-today"`) {
		t.Fatal("missing page-today")
	}
	if strings.Contains(global, `id="page-home"`) {
		t.Fatal("/today must not be the launcher")
	}
	assertNavActive(t, global, "Today")
	nav := navLinksChunk(t, global)
	todayAt := strings.Index(nav, ">Today</a>")
	projectsAt := strings.Index(nav, ">Projects</a>")
	if todayAt < 0 || projectsAt < 0 || todayAt > projectsAt {
		t.Fatalf("Today must precede Projects in global nav")
	}
	tab := tabBarChunk(t, global)
	if !strings.Contains(tab, ">Projects</a>") {
		t.Fatal("global phone tab bar must keep Projects")
	}
	if !strings.Contains(tab, ">Today</a>") {
		t.Fatal("global phone tab bar missing Today")
	}

	scoped := getBody(t, h, "/projects/proj/today")
	if !strings.Contains(scoped, `id="page-today"`) {
		t.Fatal("workspace today missing marker")
	}
	assertNavActive(t, scoped, "Today")
	wsTab := tabBarChunk(t, scoped)
	if !strings.Contains(wsTab, ">Overview</a>") {
		t.Fatal("workspace phone tab bar must keep Overview")
	}
	if !strings.Contains(scoped, "scoped=1") {
		t.Fatal("workspace live-region must refresh scoped")
	}
	if !strings.Contains(scoped, `hx-trigger="sse:ship, sse:cases, sse:history, sse:inbox"`) {
		t.Fatal("today live-region missing composed SSE trigger")
	}
	if strings.Contains(scoped, `sse:dashboard`) && strings.Contains(scoped, `id="live-today"`) {
		live := scoped[strings.Index(scoped, `id="live-today"`):]
		if i := strings.Index(live, `hx-trigger="sse:dashboard`); i >= 0 && i < 400 {
			t.Fatal("today must not listen to dashboard")
		}
	}

	home := getBody(t, h, "/")
	if !strings.Contains(home, `id="page-home"`) {
		t.Fatal("/ must remain the launcher")
	}
}

func tabBarChunk(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="tab-bar"`)
	if start < 0 {
		t.Fatal("tab-bar missing")
	}
	end := strings.Index(body[start:], `</div>`)
	if end < 0 {
		t.Fatal("tab-bar unclosed")
	}
	return body[start : start+end]
}

func TestTodayPartialHasNoLayoutChrome(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/partials/today/list", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="today-list"`) {
		t.Fatal("partial missing today-list")
	}
	for _, ban := range []string{`id="sse-status"`, "<nav", "/static/htmx.min.js"} {
		if strings.Contains(body, ban) {
			t.Fatalf("partial leaked %q", ban)
		}
	}
}

func TestTodayQueryProjectDoesNotScopeNav(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/today?project=proj")
	if !strings.Contains(body, `id="page-today"`) {
		t.Fatal("missing page")
	}
	if !strings.Contains(body, `data-scope=""`) && !strings.Contains(body, `data-scope="proj"`) {
		// Must have a data-scope attribute on side-nav.
	}
	start := strings.Index(body, `id="side-nav"`)
	if start < 0 {
		t.Fatal("side-nav missing")
	}
	chunk := body[start:]
	if i := strings.Index(chunk, "data-scope="); i < 0 {
		t.Fatal("data-scope missing")
	} else {
		attr := chunk[i : i+20]
		if strings.Contains(attr, `data-scope="proj"`) {
			t.Fatal("?project= on /today must not scope the sidebar")
		}
	}
}

func TestTodayUnauthorizedPathVsFilter(t *testing.T) {
	srv := twoProjectAuthServer(t)
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	req := httptest.NewRequest(http.MethodGet, "/projects/secret/today", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("secret workspace today status=%d want 403", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/today?project=secret", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unauthorized ?project= status=%d want 200", w.Code)
	}
}

func TestTodayHidesForbiddenProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	now := time.Now().UTC()
	if err := srv.sessions.Set("th-public", sessionstore.Entry{
		SessionID: "sess-th-public", Project: "public", OwnerID: "member-1",
		Goal:          "visible-goal",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "public q?"}},
		UpdatedAt:     now.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-secret", sessionstore.Entry{
		SessionID: "sess-th-secret", Project: "secret", OwnerID: "member-1",
		Goal: "secret-goal", CaseKey: "SECRET-1",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "secret q?"}},
		UpdatedAt:     now.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}
	req := httptest.NewRequest(http.MethodGet, "/today", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "visible-goal") {
		t.Fatal("missing visible row")
	}
	if strings.Contains(body, "secret-goal") || strings.Contains(body, "SECRET-1") {
		t.Fatal("forbidden project leaked into Today")
	}
	code, got, countsBody := getNavCounts(t, srv, "/partials/nav/counts", cookie)
	if code != http.StatusOK {
		t.Fatalf("counts=%d %s", code, countsBody)
	}
	if got["today"] != 1 {
		t.Fatalf("today count=%d want 1 (secret excluded): %v", got["today"], got)
	}
}

func TestTodayInboxHeaderWhenUnread(t *testing.T) {
	srv, cfg, _ := testServer(t)
	cfg.WebAuth = &config.WebAuthConfig{Enabled: true, SessionSecret: "test-session-secret-32-bytes-long!"}
	pc := cfg.Projects["proj"]
	pc.AllowedUserIDs = append(pc.AllowedUserIDs, "oidc:alice")
	cfg.Projects["proj"] = pc
	if err := srv.bot.QueueInbox("oidc:alice", inbox.KindRunDone, "done", "", "/sessions/w_1", "w_1", "proj"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("oidc:alice", "Alice", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/today", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "unread in Inbox") {
		t.Fatal("missing inbox header link")
	}
	req = httptest.NewRequest(http.MethodGet, "/inbox", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("inbox status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/today"`) {
		t.Fatal("inbox copy should point at Today")
	}
}

func TestTodayNavCountZeroJSON(t *testing.T) {
	srv, _, _ := testServer(t)
	code, got, body := getNavCounts(t, srv, "/partials/nav/counts", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d %s", code, body)
	}
	if _, ok := got["today"]; !ok {
		t.Fatalf("today key missing: %s", body)
	}
	if got["today"] != 0 {
		t.Fatalf("today=%d want 0: %s", got["today"], body)
	}
}
