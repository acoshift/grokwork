package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
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

func TestTodayAuthOffHidesSessions(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("th-owned", sessionstore.Entry{
		Project: "proj", OwnerID: "u0", Goal: "auth-off should not list this",
		Label: sessionstore.LabelInProgress, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.Handler(), "/today")
	if strings.Contains(body, `id="today-sessions"`) {
		t.Fatal("auth-off Today must not dump host sessions")
	}
	if strings.Contains(body, "auth-off should not list this") {
		t.Fatal("owned session leaked onto unsigned Today")
	}
	if !strings.Contains(body, "Sign in to see your queue") {
		t.Fatal("auth-off empty copy should ask to sign in")
	}
}

func TestTodayShowsMyActiveSessions(t *testing.T) {
	srv, _, _ := authOnServer(t)
	now := time.Now().UTC()
	recent := now.Format(time.RFC3339)
	stale := now.Add(-48 * time.Hour).Format(time.RFC3339)
	seed := map[string]sessionstore.Entry{
		"th-mine": {
			Project: "proj", Goal: "owned active work", OwnerID: "member-1",
			UpdatedAt: recent, Label: sessionstore.LabelOpen,
		},
		"th-co": {
			Project: "proj", Goal: "co-owned active work", OwnerID: "allow-user",
			CoOwnerIDs: []string{"member-1"}, UpdatedAt: recent, Label: sessionstore.LabelInProgress,
		},
		"th-theirs": {
			Project: "proj", Goal: "someone else's session", OwnerID: "allow-user",
			UpdatedAt: recent, Label: sessionstore.LabelOpen,
		},
		"th-engineer": {
			Project: "proj", Goal: "engineered not owned", Mode: "case",
			Phase: sessionstore.PhaseFixing, OwnerID: "allow-user", EngineerID: "member-1",
			UpdatedAt: recent, Label: sessionstore.LabelInProgress,
		},
		"th-done-stale": {
			Project: "proj", Goal: "finished last week", OwnerID: "member-1",
			UpdatedAt: stale, Label: sessionstore.LabelDone,
		},
		"th-ask": {
			Project: "proj", Goal: "throwaway ask", OwnerID: "member-1",
			SessionKind: sessionstore.SessionKindPRAsk, UpdatedAt: recent,
			Label: sessionstore.LabelOpen,
		},
		"th-untitled": {
			Project: "proj", OwnerID: "member-1", UpdatedAt: recent,
			Label: sessionstore.LabelOpen,
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	const secretPrompt = "SECRET CUSTOMER PROMPT"
	if err := srv.history.Append("th-untitled", history.Turn{
		User: "member-1", Prompt: secretPrompt, Response: "ok", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/today")
	if !strings.Contains(body, `id="today-sessions"`) {
		t.Fatal("missing your-sessions section")
	}
	if !strings.Contains(body, "owned active work") {
		t.Fatal("missing owned session")
	}
	if !strings.Contains(body, "co-owned active work") {
		t.Fatal("missing co-owned session")
	}
	if !strings.Contains(body, "/sessions/th-untitled") {
		t.Fatal("untitled owned session should still link")
	}
	if strings.Contains(body, secretPrompt) {
		t.Fatal("last prompt leaked onto Today")
	}
	for _, ban := range []string{"someone else's session", "engineered not owned", "finished last week", "throwaway ask"} {
		if strings.Contains(body, ban) {
			t.Fatalf("must not list %q", ban)
		}
	}
	if !strings.Contains(body, "Nothing waiting on you") {
		t.Fatal("healthy sessions should not look like an empty Today")
	}
	if !strings.Contains(body, `href="/sessions?owner=mine"`) {
		t.Fatal("missing all-sessions link")
	}

	scoped := getPageBody(t, srv, sid, "/projects/proj/today")
	if !strings.Contains(scoped, "owned active work") {
		t.Fatal("workspace Today missing owned session")
	}
	if !strings.Contains(scoped, `href="/projects/proj/sessions?owner=mine"`) {
		t.Fatal("workspace Today should link to scoped sessions")
	}

	code, got, countsBody := getNavCounts(t, srv, "/partials/nav/counts", &http.Cookie{Name: sessionCookieName, Value: sid})
	if code != http.StatusOK {
		t.Fatalf("counts=%d %s", code, countsBody)
	}
	if got["today"] != 0 {
		t.Fatalf("nav pill=%d want 0 (healthy sessions are not waiting): %v", got["today"], got)
	}
}

func TestTodayShowsRunningSessionEvenIfSettled(t *testing.T) {
	srv, _, _ := authOnServer(t)
	stale := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if err := srv.sessions.Set("th-run", sessionstore.Entry{
		Project: "proj", Goal: "still running after merge", OwnerID: "member-1",
		UpdatedAt: stale, Label: sessionstore.LabelNeedsReview,
		PRs: []sessionstore.TrackedPR{{
			Number: 1, State: "MERGED", Owner: "acme", Repo: "proj",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-shipped", sessionstore.Entry{
		Project: "proj", Goal: "merged last week", OwnerID: "member-1",
		UpdatedAt: stale, Label: sessionstore.LabelNeedsReview,
		PRs: []sessionstore.TrackedPR{{
			Number: 2, State: "MERGED", Owner: "acme", Repo: "proj",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cancel, err := srv.bot.InjectActiveRunForTest("th-run", "proj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/today")
	if !strings.Contains(body, "still running after merge") {
		t.Fatal("live run must stay on Today even when the PR is terminal and stale")
	}
	if !strings.Contains(body, `class="badge live">running</span>`) {
		t.Fatal("missing running badge")
	}
	if strings.Contains(body, "merged last week") {
		t.Fatal("stale shipped session without a live run must drop off")
	}

	partial := httptest.NewRequest(http.MethodGet, "/partials/today/list", nil)
	partial.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	partial.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, partial)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "still running after merge") {
		t.Fatal("live-region partial dropped the running session")
	}
}

func TestTodayActiveSessionsHideForbiddenProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := srv.sessions.Set("sess-public", sessionstore.Entry{
		Project: "public", OwnerID: "member-1", Goal: "public-active-session",
		Label: sessionstore.LabelInProgress, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("sess-secret", sessionstore.Entry{
		Project: "secret", OwnerID: "member-1", Goal: "secret-active-session",
		Label: sessionstore.LabelInProgress, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/today")
	if !strings.Contains(body, "public-active-session") {
		t.Fatal("missing visible active session")
	}
	if strings.Contains(body, "secret-active-session") {
		t.Fatal("forbidden project session leaked into Today")
	}
}

func TestClipTodaySessionsCapRunningFirstAndEmptyActor(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rows := make([]history.Summary, 0, 42)
	for i := range 41 {
		rows = append(rows, history.Summary{
			ThreadID:  fmt.Sprintf("s-%02d", i),
			Project:   "proj",
			OwnerID:   "u1",
			Goal:      fmt.Sprintf("g-%02d", i),
			Label:     sessionstore.LabelOpen,
			UpdatedAt: now.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339),
		})
	}
	rows = append(rows, history.Summary{
		ThreadID:  "running-old",
		Project:   "proj",
		OwnerID:   "u1",
		Goal:      "live run",
		Label:     sessionstore.LabelOpen,
		Running:   true,
		UpdatedAt: now.Add(-48 * time.Hour).UTC().Format(time.RFC3339),
	})
	f := sessionFilters{State: "active", Owner: sessionOwnerMine, ViewerID: "u1"}
	got, matched := clipTodaySessions(rows, f, now)
	if matched != 42 {
		t.Fatalf("matched=%d want 42", matched)
	}
	if len(got) != todaySessionCap {
		t.Fatalf("shown=%d want %d", len(got), todaySessionCap)
	}
	if got[0].ThreadID != "running-old" {
		t.Fatalf("running should sort first: %s", got[0].ThreadID)
	}
	if got[1].ThreadID != "s-40" {
		t.Fatalf("newest idle after running: %s", got[1].ThreadID)
	}

	empty, n := clipTodaySessions(rows, sessionFilters{
		State: "active", Owner: sessionOwnerMine, ViewerID: "",
	}, now)
	if n != 0 || len(empty) != 0 {
		t.Fatalf("empty actor matched=%d shown=%d", n, len(empty))
	}

	other, n := clipTodaySessions(rows, sessionFilters{
		State: "active", Owner: sessionOwnerMine, ViewerID: "other",
	}, now)
	if n != 0 || len(other) != 0 {
		t.Fatalf("stranger matched=%d shown=%d", n, len(other))
	}
}
