package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func getNavCounts(t *testing.T, srv *Server, path string, cookie *http.Cookie) (int, map[string]int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		return w.Code, nil, body
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("json: %v body=%s", err, body)
	}
	out := make(map[string]int, len(raw))
	for k, v := range raw {
		n, ok := v.(float64)
		if !ok {
			t.Fatalf("key %q = %T %v", k, v, v)
		}
		out[k] = int(n)
	}
	return w.Code, out, body
}

func TestNavCountPlaceholders(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	nav := navLinksChunk(t, body)
	for _, key := range []string{"ship", "cases", "reviews", "inbox"} {
		needle := `data-nav-count="` + key + `"`
		if !strings.Contains(nav, needle) {
			t.Fatalf("global nav missing %s", needle)
		}
	}
	if strings.Contains(nav, `data-nav-count="issues"`) {
		t.Fatal("global nav must not badge Issues")
	}
	if strings.Contains(nav, `data-nav-count="errors"`) {
		t.Fatal("global nav must not badge Errors")
	}
	if !strings.Contains(body, `data-counts="/partials/nav/counts"`) {
		t.Fatal("side-nav missing counts URL")
	}

	req = httptest.NewRequest(http.MethodGet, "/projects/proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body = w.Body.String()
	nav = navLinksChunk(t, body)
	for _, key := range []string{"ship", "cases", "reviews", "issues"} {
		needle := `data-nav-count="` + key + `"`
		if !strings.Contains(nav, needle) {
			t.Fatalf("workspace nav missing %s", needle)
		}
	}
	if strings.Contains(nav, `data-nav-count="errors"`) {
		t.Fatal("errors badge without a source enabled")
	}
	assertNavActive(t, body, "Overview")
}

// TestNavOobSkipsSameScope pins the shell contract that keeps count pills
// mounted: hx-select-oob still offers #side-nav (scope change must remount),
// but same-scope boost cancels that swap and does not refetch counts.
func TestNavOobSkipsSameScope(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/")
	for _, want := range []string{
		`hx-select-oob="#side-nav"`,
		`htmx:oobBeforeSwap`,
		`d.shouldSwap = false`,
		`e.preventDefault()`,
		`htmx:oobAfterSwap`,
		`function oobNavElement(frag)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("layout missing %q", want)
		}
	}
	if strings.Contains(body, "if (e.detail && e.detail.boosted) loadNavCounts()") {
		t.Fatal("same-scope boosted nav must not refetch counts")
	}
}

// TestNavCountsLiveOnSSE pins the path that refreshes sidebar pills when a
// PR merges (sse:ship), a case board count moves (sse:cases), or an inbox
// unread count moves (sse:inbox): EventSource named events plus reconnect
// catch-up, local JSON only (no GitHub re-hit).
func TestNavCountsLiveOnSSE(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/")
	for _, want := range []string{
		`if (name === "ship" || name === "cases" || name === "inbox") loadNavCounts(true)`,
		`if (changed.ship || changed.cases || changed.inbox) loadNavCounts(true)`,
		`function loadNavCounts(localOnly)`,
		`if (localOnly) return`,
		`if (remote && key !== "issues" && key !== "errors") return`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("layout missing %q", want)
		}
	}
	if strings.Contains(body, "if (e.detail && e.detail.boosted) loadNavCounts()") {
		t.Fatal("same-scope boosted nav must not refetch counts")
	}
}

func navLinksChunk(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="nav-links"`)
	end := strings.Index(body, `id="tab-bar"`)
	if start < 0 || end < start {
		t.Fatalf("nav-links/tab-bar markers missing")
	}
	return body[start:end]
}

func TestNavCountPlaceholdersErrors(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectErrorsDeploys("proj", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.Handler(), "/projects/proj")
	if !strings.Contains(navLinksChunk(t, body), `data-nav-count="errors"`) {
		t.Fatal("workspace nav missing errors badge")
	}
}

func TestNavCountsLocalBoards(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("th-open-pr", sessionstore.Entry{
		SessionID: "s-pr", Project: "proj",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/1", Number: 1, State: "OPEN",
			Title: "fix", Owner: "acme", Repo: "app",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-merged-pr", sessionstore.Entry{
		SessionID: "s-merged", Project: "proj",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/2", Number: 2, State: "MERGED",
			Title: "done", Owner: "acme", Repo: "app",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-case-open", sessionstore.Entry{
		SessionID: "s-case", Project: "proj", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "PROJ-1", CustomerTitle: "Checkout",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-case-closed", sessionstore.Entry{
		SessionID: "s-closed", Project: "proj", Mode: "case", Phase: sessionstore.PhaseClosed,
		CaseKey: "PROJ-2", CustomerTitle: "Typo",
	}); err != nil {
		t.Fatal(err)
	}

	code, got, body := getNavCounts(t, srv, "/partials/nav/counts", nil)
	if code != http.StatusOK {
		t.Fatalf("global status=%d body=%s", code, body)
	}
	if _, ok := got["inbox"]; !ok {
		t.Fatalf("inbox key missing: %s", body)
	}
	if got["inbox"] != 0 {
		t.Fatalf("global inbox=%d want 0 (%v)", got["inbox"], got)
	}
	if got["ship"] != 1 {
		t.Fatalf("global ship=%d want 1 (%v)", got["ship"], got)
	}
	if got["cases"] != 1 {
		t.Fatalf("global cases=%d want 1 (%v)", got["cases"], got)
	}
	if _, ok := got["issues"]; ok {
		t.Fatalf("global must omit issues: %v", got)
	}
	if _, ok := got["errors"]; ok {
		t.Fatalf("global must omit errors: %v", got)
	}

	code, got, body = getNavCounts(t, srv, "/partials/nav/counts?project=proj", nil)
	if code != http.StatusOK {
		t.Fatalf("project status=%d body=%s", code, body)
	}
	if got["ship"] != 1 || got["cases"] != 1 {
		t.Fatalf("project counts=%v", got)
	}
	if _, ok := got["issues"]; ok {
		t.Fatalf("local fetch must omit issues: %v", got)
	}
}

func TestNavCountsHidesUnauthorizedProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.sessions.Set("case-public", sessionstore.Entry{
		SessionID: "s-pub-case", Project: "public", Mode: "case",
		Phase: sessionstore.PhaseIntake, CaseKey: "PUBLIC-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("case-secret", sessionstore.Entry{
		SessionID: "s-sec-case", Project: "secret", Mode: "case",
		Phase: sessionstore.PhaseIntake, CaseKey: "SECRET-1",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	code, got, body := getNavCounts(t, srv, "/partials/nav/counts", cookie)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if got["ship"] != 1 {
		t.Fatalf("member global ship=%d want 1 (public only): %v", got["ship"], got)
	}
	if got["cases"] != 1 {
		t.Fatalf("member global cases=%d want 1: %v", got["cases"], got)
	}

	code, _, body = getNavCounts(t, srv, "/partials/nav/counts?project=secret", cookie)
	if code != http.StatusForbidden {
		t.Fatalf("secret project status=%d body=%s", code, body)
	}
}

func TestNavCountsPendingReviews(t *testing.T) {
	srv := twoProjectAuthServer(t)
	store := srv.bot.Reviews()
	if store == nil {
		t.Fatal("review store")
	}
	if _, err := store.RequestReview(reviewstore.Request{
		Owner: "acme", Repo: "public", Number: 1, Project: "public",
		RequesterID: "admin-1", ReviewerID: "member-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestReview(reviewstore.Request{
		Owner: "acme", Repo: "secret", Number: 2, Project: "secret",
		RequesterID: "admin-1", ReviewerID: "member-1",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	_, got, _ := getNavCounts(t, srv, "/partials/nav/counts", cookie)
	if got["reviews"] != 1 {
		t.Fatalf("global reviews=%d want 1 (secret hidden): %v", got["reviews"], got)
	}
	_, got, _ = getNavCounts(t, srv, "/partials/nav/counts?project=public", cookie)
	if got["reviews"] != 1 {
		t.Fatalf("public reviews=%d want 1: %v", got["reviews"], got)
	}
}

func TestNavCountsIssues(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	hits := 0
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "issue list") {
			hits++
			return []byte(`[
				{"number":7,"url":"https://github.com/acme/app/issues/7","title":"A","state":"OPEN","author":{"login":"a"}},
				{"number":8,"url":"https://github.com/acme/app/issues/8","title":"B","state":"OPEN","author":{"login":"b"}}
			]`), nil
		}
		return []byte("[]"), nil
	}
	_, got, _ := getNavCounts(t, srv, "/partials/nav/counts?project=proj", nil)
	if _, ok := got["issues"]; ok {
		t.Fatalf("local fetch must omit issues: %v", got)
	}
	if hits != 0 {
		t.Fatalf("local fetch hit GitHub %d times", hits)
	}
	code, got, body := getNavCounts(t, srv, "/partials/nav/counts?project=proj&remote=1", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if got["issues"] != 2 {
		t.Fatalf("issues=%d want 2: %v", got["issues"], got)
	}
	if hits == 0 {
		t.Fatal("remote fetch never listed issues")
	}
}

func TestNavCountsErrors(t *testing.T) {
	srv, _ := deploysErrorsFixture(t)
	code, got, body := getNavCounts(t, srv, "/partials/nav/counts?project=proj&remote=1", nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if got["errors"] != 1 {
		t.Fatalf("errors=%d want 1: %v body=%s", got["errors"], got, body)
	}
}

func TestNavCountsAuthRequired(t *testing.T) {
	srv, _, _ := authOnServer(t)
	req := httptest.NewRequest(http.MethodGet, "/partials/nav/counts", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status=%d want 302", w.Code)
	}
}
