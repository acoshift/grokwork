package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// getSearch issues a GET as the given cookie (nil = auth-off server).
func getSearch(t *testing.T, srv *Server, path string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestSearchShellBoxAndPageMarkers pins the chrome contract: the box is rendered
// by the layout on ordinary pages (so it is reachable from anywhere), and the
// results page carries its id="page-*" marker.
func TestSearchShellBoxAndPageMarkers(t *testing.T) {
	srv, _, _ := testServer(t)

	for _, path := range []string{"/", "/sessions", "/projects/proj"} {
		code, body := getSearch(t, srv, path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s status=%d", path, code)
		}
		for _, want := range []string{`id="nav-search"`, `action="/search"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing shell search %q", path, want)
			}
		}
	}

	code, body := getSearch(t, srv, "/search", nil)
	if code != http.StatusOK {
		t.Fatalf("/search status=%d", code)
	}
	for _, want := range []string{`id="page-search"`, `id="search-form"`, `id="search-empty"`, `id="search-bounds"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/search missing %q", want)
		}
	}
	// The results page is a full document, not a fragment: it renders inside the
	// shell like every other lead view.
	if !strings.Contains(body, `id="live-root"`) {
		t.Fatal("/search is not rendered in the shell")
	}
}

// TestSearchFindsEveryKind covers the four in-memory kinds and their hrefs.
func TestSearchFindsEveryKind(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("thread-pr", sessionstore.Entry{
		Project: "proj",
		Goal:    "rewrite the checkout ledger",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/42", Number: 42, State: "OPEN",
			Title: "ledger rounding", Owner: "acme", Repo: "app",
		}},
		Issues: []sessionstore.TrackedIssue{{
			Number: 7, Owner: "acme", Repo: "app",
			URL: "https://github.com/acme/app/issues/7",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("thread-case", sessionstore.Entry{
		Project: "proj", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "PROJ-1", CustomerTitle: "ledger shows a stale balance",
		CustomerRef: "ZD-4821", Severity: "high",
	}); err != nil {
		t.Fatal(err)
	}

	// Free text spans cases, sessions and PR titles at once.
	_, body := getSearch(t, srv, "/search?q=ledger", nil)
	for _, want := range []string{
		`id="search-group-case"`,
		`id="search-group-session"`,
		`id="search-group-pr"`,
		"ledger shows a stale balance",
		"rewrite the checkout ledger",
		"ledger rounding",
		`href="/prs/acme/app/42?project=proj"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("search ledger missing %q\n%s", want, body)
		}
	}

	// Their ticket id, not ours: CustomerRef is searchable too.
	if _, b := getSearch(t, srv, "/search?q=ZD-4821", nil); !strings.Contains(b, "ledger shows a stale balance") {
		t.Fatal("customer ref does not resolve")
	}

	// A tracked issue by number, and by owner/repo#n.
	for _, q := range []string{"%237", "acme%2Fapp%237"} {
		_, b := getSearch(t, srv, "/search?q="+q, nil)
		if !strings.Contains(b, `id="search-group-issue"`) {
			t.Fatalf("issue query %q found nothing\n%s", q, b)
		}
		if !strings.Contains(b, `href="/projects/proj/issues/7?owner=acme&amp;repo=app"`) {
			t.Fatalf("issue query %q missing in-app href", q)
		}
	}

	// A thread id is quotable too (it is what Discord and the logs print).
	if _, b := getSearch(t, srv, "/search?q=thread-pr", nil); !strings.Contains(b, "rewrite the checkout ledger") {
		t.Fatal("thread id does not resolve")
	}

	// Nothing matched is a stated outcome, not an empty page.
	_, b := getSearch(t, srv, "/search?q=zzz-nothing-here", nil)
	if !strings.Contains(b, `id="search-empty"`) || strings.Contains(b, `class="sr-row"`) {
		t.Fatalf("no-match search should render the empty state\n%s", b)
	}
}

// TestSearchFoldsOneRecordTrackedTwice: two units bound to the same PR are two
// sessions but one pull request, and the results say so once.
func TestSearchFoldsOneRecordTrackedTwice(t *testing.T) {
	srv, _, _ := testServer(t)
	pr := sessionstore.TrackedPR{
		URL: "https://github.com/acme/app/pull/42", Number: 42, State: "OPEN",
		Title: "ledger rounding", Owner: "acme", Repo: "app",
	}
	for _, tid := range []string{"thread-a", "thread-b"} {
		if err := srv.sessions.Set(tid, sessionstore.Entry{
			Project: "proj", Goal: "work on " + tid, PRs: []sessionstore.TrackedPR{pr},
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, body := getSearch(t, srv, "/search?q=rounding", nil)
	if got := strings.Count(body, `href="/prs/acme/app/42?project=proj"`); got != 1 {
		t.Fatalf("PR rendered %d times, want 1\n%s", got, body)
	}
	// The count above the group has to agree with the rows under it.
	if !strings.Contains(body, `<h2 class="sr-h">Pull requests <span class="sr-n">1</span></h2>`) {
		t.Fatalf("group count did not fold with the rows\n%s", body)
	}
}

// TestSearchCaseKeyJumpsToTheCase: an exact key is a reference, so it resolves
// the way /c/{key} does instead of rendering one row to click.
func TestSearchCaseKeyJumpsToTheCase(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("thread-case", sessionstore.Entry{
		Project: "proj", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "PROJ-14", CustomerTitle: "checkout 500s",
	}); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive and whitespace-tolerant, like every other reference.
	for _, q := range []string{"PROJ-14", "proj-14", "%20proj-14%20"} {
		req := httptest.NewRequest(http.MethodGet, "/search?q="+q, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("q=%s status=%d want 302", q, w.Code)
		}
		loc := w.Header().Get("Location")
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatal(err)
		}
		if u.Path != "/sessions/thread-case" {
			t.Fatalf("q=%s location=%s", q, loc)
		}
		if u.Query().Get("project") != "proj" {
			t.Fatalf("q=%s lost the project scope: %s", q, loc)
		}
		// Provenance: the case's ← crumb must come back to these results.
		back := u.Query().Get("back")
		if !strings.HasPrefix(back, "/search?") {
			t.Fatalf("q=%s back=%q", q, back)
		}
		if _, label, ok := resolveBackLink(back); !ok || label != "Search" {
			t.Fatalf("q=%s back link does not resolve: %q", q, back)
		}
	}

	// A key that is well-formed but unassigned is an ordinary (empty) search,
	// never a redirect.
	code, body := getSearch(t, srv, "/search?q=PROJ-99", nil)
	if code != http.StatusOK {
		t.Fatalf("unassigned key status=%d want 200", code)
	}
	if !strings.Contains(body, `id="search-empty"`) {
		t.Fatal("unassigned key should fall through to results")
	}
}

// TestSearchIsACLFilteredBeforeRanking is the containment rule: a term that
// only matches a project the viewer cannot open returns nothing at all — no
// title, no count, no thread id.
func TestSearchIsACLFilteredBeforeRanking(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.sessions.Set("case-secret", sessionstore.Entry{
		Project: "secret", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "SECRET-1", CustomerTitle: "zebra outage", CustomerRef: "ZD-9",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/secret/pull/3", Number: 3, State: "OPEN",
			Title: "zebra fix", Owner: "acme", Repo: "secret",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("case-public", sessionstore.Entry{
		Project: "public", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "PUBLIC-1", CustomerTitle: "public zebra note",
	}); err != nil {
		t.Fatal(err)
	}

	memberSID, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	member := &http.Cookie{Name: sessionCookieName, Value: memberSID}
	adminSID, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	admin := &http.Cookie{Name: sessionCookieName, Value: adminSID}

	// The admin proves the term really does match the hidden project.
	if _, b := getSearch(t, srv, "/search?q=zebra", admin); !strings.Contains(b, "zebra outage") ||
		!strings.Contains(b, "zebra fix") || !strings.Contains(b, "public zebra note") {
		t.Fatalf("admin search should span both projects\n%s", b)
	}

	code, body := getSearch(t, srv, "/search?q=zebra", member)
	if code != http.StatusOK {
		t.Fatalf("member search status=%d", code)
	}
	if !strings.Contains(body, "public zebra note") {
		t.Fatal("member lost their own project's match")
	}
	for _, leak := range []string{"zebra outage", "zebra fix", "case-secret", "SECRET-1", "ZD-9", "acme/secret"} {
		if strings.Contains(body, leak) {
			t.Fatalf("member search leaked %q from a hidden project\n%s", leak, body)
		}
	}

	// ?project= naming a hidden project must not become a read either — it is a
	// data filter, and the filter can only narrow what is already visible.
	_, body = getSearch(t, srv, "/search?q=zebra&project=secret", member)
	for _, leak := range []string{"zebra outage", "zebra fix", "case-secret"} {
		if strings.Contains(body, leak) {
			t.Fatalf("project filter leaked %q\n%s", leak, body)
		}
	}
	// The dropdown itself lists only projects the viewer may open.
	if strings.Contains(body, `<option value="secret"`) {
		t.Fatalf("project filter offers a hidden project\n%s", body)
	}
}

// TestSearchFiltersBeforeItRanks is the sharp form of the ACL rule, and the one
// a "filter the ranked list" implementation fails while still hiding every
// title: a hidden project full of *better* matches must not evict the viewer's
// single worse one out of a capped group, and must not be counted.
func TestSearchFiltersBeforeItRanks(t *testing.T) {
	srv := twoProjectAuthServer(t)
	// searchKindCap exact matches in the project the member cannot open …
	for i := range searchKindCap + 5 {
		if err := srv.sessions.Set(fmt.Sprintf("hidden-%02d", i), sessionstore.Entry{
			Project: "secret", Goal: "evict",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// … against one weaker substring match in the project they can.
	if err := srv.sessions.Set("visible-1", sessionstore.Entry{
		Project: "public", Goal: "please evict this one row",
	}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_, body := getSearch(t, srv, "/search?q=evict", &http.Cookie{Name: sessionCookieName, Value: sid})
	// Ranking the whole store first would fill all searchKindCap slots with the
	// exact matches and leave this row on the cutting-room floor.
	if !strings.Contains(body, "please evict this one row") {
		t.Fatalf("a hidden project's better matches evicted the viewer's own row\n%s", body)
	}
	if got := strings.Count(body, `class="sr-row"`); got != 1 {
		t.Fatalf("rendered %d rows, want exactly the 1 visible match", got)
	}
	// The count is drawn from the same filtered set — 26 here would disclose how
	// much work the hidden project has, without naming any of it.
	if !strings.Contains(body, `<h2 class="sr-h">Sessions <span class="sr-n">1</span></h2>`) {
		t.Fatalf("group total counts rows the viewer may not see\n%s", body)
	}
	if strings.Contains(body, "hidden-") {
		t.Fatal("hidden thread ids leaked")
	}
}

// TestSearchForbiddenCaseKeyLooksExactlyLikeAMissingOne: the jump must not turn
// the key space into a probe. A key that exists in a hidden project and a key
// that exists nowhere have to produce the same answer, byte for byte, once the
// key the user typed is accounted for.
func TestSearchForbiddenCaseKeyLooksExactlyLikeAMissingOne(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.sessions.Set("case-secret", sessionstore.Entry{
		Project: "secret", Mode: "case", Phase: sessionstore.PhaseIntake,
		CaseKey: "SECRET-1", CustomerTitle: "hidden outage",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	forbiddenCode, forbidden := getSearch(t, srv, "/search?q=SECRET-1", cookie)
	missingCode, missing := getSearch(t, srv, "/search?q=SECRET-2", cookie)
	if forbiddenCode != http.StatusOK || missingCode != http.StatusOK {
		t.Fatalf("forbidden=%d missing=%d — a redirect or an error distinguishes them",
			forbiddenCode, missingCode)
	}
	if got := strings.ReplaceAll(forbidden, "SECRET-1", "SECRET-2"); got != missing {
		t.Fatal("a forbidden case key answers differently from a missing one")
	}
	if strings.Contains(forbidden, "hidden outage") || strings.Contains(forbidden, "case-secret") {
		t.Fatal("forbidden key leaked the case")
	}

	// An admin, for whom the key is neither missing nor forbidden, does jump —
	// otherwise this test would pass on a search that never redirects at all.
	adminSID, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/search?q=SECRET-1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminSID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("admin key jump status=%d want 302", w.Code)
	}
}

// TestSearchCapsResultsPerKind: the cap is enforced and, more importantly,
// declared — a truncated list that does not say so reads as the whole answer.
func TestSearchCapsResultsPerKind(t *testing.T) {
	srv, _, _ := testServer(t)
	const seeded = searchKindCap + 5
	for i := range seeded {
		if err := srv.sessions.Set(fmt.Sprintf("thread-cap-%02d", i), sessionstore.Entry{
			Project: "proj",
			Goal:    fmt.Sprintf("capme widget %02d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, body := getSearch(t, srv, "/search?q=capme", nil)
	if got := strings.Count(body, `class="sr-row"`); got != searchKindCap {
		t.Fatalf("rendered %d rows, want the cap of %d", got, searchKindCap)
	}
	for _, want := range []string{
		fmt.Sprintf("Showing the first %d of %d", searchKindCap, seeded),
		fmt.Sprintf("Each kind returns at most %d matches", searchKindCap),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cap not declared: missing %q\n%s", want, body)
		}
	}
}

// TestSearchCommitsAreBoundedAndScoped: commits need a project, and the page
// says what window it read rather than pretending it read everything.
func TestSearchCommitsAreBoundedAndScoped(t *testing.T) {
	srv, _, _ := testServer(t)

	_, global := getSearch(t, srv, "/search?q=anything", nil)
	if !strings.Contains(global, "Commits: not searched — "+searchNoProjectNote) {
		t.Fatalf("global search should say commits were not read\n%s", global)
	}
	if !strings.Contains(global, fmt.Sprintf("newest %d", searchCommitScan)) &&
		!strings.Contains(global, "not searched") {
		t.Fatalf("commit bound not stated\n%s", global)
	}

	// The fixture project is not a git checkout: that is reported inline, not as
	// an error page and not as "no commits match".
	code, scoped := getSearch(t, srv, "/search?q=anything&project=proj", nil)
	if code != http.StatusOK {
		t.Fatalf("project-scoped search status=%d", code)
	}
	if !strings.Contains(scoped, "Commits: not searched") {
		t.Fatalf("unreadable repo should be reported\n%s", scoped)
	}
}

// TestSearchCommitsReadABoundedWindow exercises the one kind that touches the
// disk against a real checkout, and pins the shape of the command it runs: a
// fixed -n window and no --grep, whose walk length depends on how rare the term
// is rather than on anything this code controls.
func TestSearchCommitsReadABoundedWindow(t *testing.T) {
	srv, cfg, dir := testServer(t)
	repo := filepath.Join(dir, "proj")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	for i, subject := range []string{"init the thing", "fix(ledger): rounding on refunds", "chore: tidy"} {
		if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("f%d", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "-A")
		runGit("commit", "-m", subject)
	}
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "proj"}}); err != nil {
		t.Fatal(err)
	}

	var logArgs [][]string
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "log" {
			logArgs = append(logArgs, append([]string(nil), args...))
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		return cmd.Output()
	}

	_, body := getSearch(t, srv, "/search?q=ledger&project=proj", nil)
	if !strings.Contains(body, `id="search-group-commit"`) {
		t.Fatalf("commit search found nothing\n%s", body)
	}
	if !strings.Contains(body, "fix(ledger): rounding on refunds") {
		t.Fatal("commit subject missing from results")
	}
	if !strings.Contains(body, `href="/projects/proj/commits/`) ||
		!strings.Contains(body, "owner=acme&amp;repo=proj") {
		t.Fatalf("commit row does not link to the in-app detail page\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("newest %d", searchCommitScan)) {
		t.Fatal("bounds line does not name the commit window")
	}

	if len(logArgs) != 1 {
		t.Fatalf("ran %d git log invocations, want exactly 1: %v", len(logArgs), logArgs)
	}
	joined := strings.Join(logArgs[0], " ")
	if !strings.Contains(joined, fmt.Sprintf("-n %d", searchCommitScan)) {
		t.Fatalf("git log is not window-bounded: %q", joined)
	}
	if strings.Contains(joined, "--grep") {
		t.Fatalf("git log uses --grep, whose walk is unbounded for a rare term: %q", joined)
	}

	// A short sha resolves the commit it names.
	head, err := exec.Command("git", "-C", repo, "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	short := strings.TrimSpace(string(head))
	if _, b := getSearch(t, srv, "/search?q="+short+"&project=proj", nil); !strings.Contains(b, "chore: tidy") {
		t.Fatalf("short sha %q did not resolve\n%s", short, b)
	}

	// A global search over the same instance still reads no log at all.
	before := len(logArgs)
	getSearch(t, srv, "/search?q=ledger", nil)
	if len(logArgs) != before {
		t.Fatal("an unscoped search shelled out to git")
	}
}

// TestSearchQueryIsClamped keeps a pathological needle from turning every field
// comparison into a long scan.
func TestSearchQueryIsClamped(t *testing.T) {
	long := strings.Repeat("a", searchMaxQuery+50)
	if got := normalizeSearchQuery("  " + long + "  "); len(got) != searchMaxQuery {
		t.Fatalf("clamped to %d, want %d", len(got), searchMaxQuery)
	}
	if got := normalizeSearchQuery("  spaced  "); got != "spaced" {
		t.Fatalf("normalize=%q", got)
	}
}

// TestSearchRankingPutsIdentityMatchesFirst: a substring in someone's goal must
// not outrank the record the query names exactly.
func TestSearchRankingPutsIdentityMatchesFirst(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("thread-noise", sessionstore.Entry{
		Project: "proj", Goal: "look into acme/app#42 before shipping",
		UpdatedAt: "2030-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("thread-exact", sessionstore.Entry{
		Project: "proj", Goal: "42",
	}); err != nil {
		t.Fatal(err)
	}
	_, body := getSearch(t, srv, "/search?q=42", nil)
	exact := strings.Index(body, "thread-exact")
	noise := strings.Index(body, "thread-noise")
	if exact < 0 || noise < 0 {
		t.Fatalf("both rows should match\n%s", body)
	}
	if exact > noise {
		t.Fatal("an exact match ranked below a substring match")
	}
}
