package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// assertNavActive checks the top-nav item with the given label has class active
// (server-rendered Is* flag). Layout also has navActiveFor() for hx-boost path sync.
func assertNavActive(t *testing.T, body, label string) {
	t.Helper()
	// Template: class="{{if .IsIssues}}active{{end}}">Issues
	want := `class="active">` + label + `</a>`
	if !strings.Contains(body, want) {
		t.Fatalf("nav %q not active (want %q)", label, want)
	}
}

func workflowServer(t *testing.T) *Server {
	t.Helper()
	srv, cfg, _ := testServer(t)
	// Multi-repo catalog for proj
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{
		{Owner: "acme", Repo: "app"},
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatal(err)
	}
	// Linear on
	if err := cfg.SetProjectLinear("proj", true, "ENG", "lin-key", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDiscordChannel("proj", "ch"); err != nil {
		t.Fatal(err)
	}
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "api graphql") && strings.Contains(joined, "closedByPullRequestsReferences"):
			return []byte(`{
				"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[
					{"number":9,"title":"Fix fixture bug","url":"https://github.com/acme/app/pull/9","state":"OPEN","isDraft":false,
					 "repository":{"name":"app","owner":{"login":"acme"}}}
				]}}}}
			}`), nil
		case strings.HasPrefix(joined, "issue list"):
			if !strings.Contains(joined, "--repo acme/api") && !strings.Contains(joined, "--repo acme/app") {
				t.Fatalf("issue list missing --repo: %v", args)
			}
			repo := "app"
			if strings.Contains(joined, "acme/api") {
				repo = "api"
			}
			return []byte(`[
				{"number":7,"url":"https://github.com/acme/` + repo + `/issues/7","title":"Fixture bug ` + repo + `","state":"OPEN","author":{"login":"alice"},"labels":[],"body":"body",
				 "closedByPullRequestsReferences":[{"number":9,"url":"https://github.com/acme/` + repo + `/pull/9","repository":{"name":"` + repo + `","owner":{"login":"acme"}}}]}
			]`), nil
		case strings.Contains(joined, "issue view 7"):
			return []byte(`{
				"number":7,"url":"https://github.com/acme/app/issues/7","title":"Fixture bug app",
				"state":"OPEN","author":{"login":"alice"},"labels":[{"name":"bug"}],
				"body":"detail body","comments":[{"author":{"login":"bob"},"body":"note","url":"u"}],
				"closedByPullRequestsReferences":[{"number":9,"url":"https://github.com/acme/app/pull/9","repository":{"name":"app","owner":{"login":"acme"}}}]
			}`), nil
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"Ship feature",
				"state":"OPEN","isDraft":false,"reviewDecision":"APPROVED","headRefOid":"abc",
				"headRefName":"feat","baseRefName":"main","body":"pr body",
				"mergeable":"MERGEABLE","author":{"login":"zoe"},
				"additions":1,"deletions":0,"changedFiles":1
			}`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		case strings.HasPrefix(joined, "pr diff"):
			return []byte("diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "diff":
			return []byte("diff --git a/wt.go b/wt.go\n--- a/wt.go\n+++ b/wt.go\n@@ -1 +1 @@\n-a\n+b\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "log":
			return []byte("abcdef0123456789\x1fFixture commit\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "rev-parse":
			return []byte("abcdef0123456789abcdef0123456789abcdef01\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "show":
			// Metadata (-s), stat, or patch.
			for _, a := range args {
				if a == "-s" {
					return []byte("abcdef0123456789abcdef0123456789abcdef01\x1fFixture commit\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\x1fbody note\n"), nil
				}
				if a == "--stat" {
					return []byte(" foo.go | 1 +\n 1 file changed\n"), nil
				}
				if a == "-p" {
					return []byte("diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"), nil
				}
			}
			return nil, nil
		default:
			t.Fatalf("unexpected gh/git %s %v", name, args)
			return nil, nil
		}
	}
	srv.linearNew = func(apiKey string) *linear.Client {
		c := linear.New(apiKey)
		// RoundTripper mock via custom HTTP client
		c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			q, _ := body["query"].(string)
			var payload any
			if strings.Contains(q, "TeamIssues") {
				payload = map[string]any{
					"data": map[string]any{
						"issues": map[string]any{
							"nodes": []map[string]any{{
								"id": "1", "identifier": "ENG-1", "title": "Lin fixture",
								"url": "https://linear.app/x/issue/ENG-1", "description": "d",
								"state": map[string]string{"name": "Todo"},
								"team":  map[string]string{"key": "ENG"},
							}},
						},
					},
				}
			} else {
				payload = map[string]any{
					"data": map[string]any{
						"issues": map[string]any{
							"nodes": []map[string]any{{
								"id": "1", "identifier": "ENG-1", "title": "Lin fixture",
								"url": "https://linear.app/x/issue/ENG-1", "description": "detail desc",
								"state": map[string]string{"name": "Todo"},
								"team":  map[string]string{"key": "ENG"},
							}},
						},
					},
				}
			}
			b, _ := json.Marshal(payload)
			return &http.Response{
				StatusCode: 200,
				Body:       ioNopCloser(strings.NewReader(string(b))),
				Header:     make(http.Header),
			}, nil
		})}
		return c
	}
	return srv
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func ioNopCloser(r *strings.Reader) *nopCloser { return &nopCloser{r} }

func TestIssuesListAndDetail(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues?owner=acme&repo=api", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-issues-list"`,
		"acme/api",
		"Fixture bug api",
		"#7",
		`name="repo_full"`,
		">PRs</th>",
		// Linked PR count column (one PR in fixture).
		"<td class=\"mono\">1</td>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %q in %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/projects/proj/issues/7?owner=acme&repo=app", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d", w.Code)
	}
	body = w.Body.String()
	for _, want := range []string{
		`id="page-issue-detail"`, "Fixture bug app", "detail body", "bob", "note",
		`id="issue-linked-prs"`, "Related PRs", "Fix fixture bug",
		`/prs/acme/app/9`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
	assertNavActive(t, body, "Issues")
	// Boosted nav path rules must know issue detail lives under /projects/…/issues/…
	if !strings.Contains(body, "function navActiveFor") || !strings.Contains(body, `href === "/issues"`) {
		t.Fatal("layout missing navActiveFor path rules for Issues detail URLs")
	}
}

func TestLinearListAndDetail(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/linear", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "ENG-1") || !strings.Contains(body, "Lin fixture") {
		t.Fatalf("body=%s", body)
	}
	assertNavActive(t, body, "Issues")
	req = httptest.NewRequest(http.MethodGet, "/projects/proj/linear/ENG-1", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, "detail desc") {
		t.Fatalf("body=%s", body)
	}
	assertNavActive(t, body, "Issues")
}

func TestPRDetailAndDiff(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/prs/acme/app/9?project=proj", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pr status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`id="page-pr-detail"`, "Ship feature", "pr body", "APPROVED", "MERGEABLE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("pr missing %q", want)
		}
	}
	assertNavActive(t, body, "Ship")
	req = httptest.NewRequest(http.MethodGet, "/prs/acme/app/9/diff?project=proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("diff status=%d", w.Code)
	}
	body = w.Body.String()
	for _, want := range []string{`id="page-diff"`, "foo.go", "@@", "old", "new", "1 file(s), 1 hunk(s)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("diff missing %q", want)
		}
	}
	// '+' is HTML-escaped in text/template as &#43;
	if !strings.Contains(body, "&#43;new") && !strings.Contains(body, "+new") {
		t.Fatal("diff missing added line")
	}
}

func TestSessionDiff(t *testing.T) {
	srv := workflowServer(t)
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "s", Project: "proj", Cwd: "/tmp/wt", MainCwd: "/tmp/main",
	}); err != nil {
		t.Fatal(err)
	}
	// Mock IsRepo is not used; ghRunner still runs. Session cwd /tmp/wt is not a
	// real repo, so resolve falls through unless we plant a disk worktree or
	// make the mock path enough. Force via MainCwd project path from test config.
	// Project "proj" has a real path under the test server temp dir.
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path missing")
	}
	// init a real git repo at project path so resolveSessionDiffCwd accepts it
	// when worktree is missing (main checkout fallback).
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	var sawDir string
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			sawDir = dir
		}
		return orig(ctx, dir, name, args...)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "wt.go") || !strings.Contains(body, "@@") {
		t.Fatalf("body=%s", body)
	}
	if sawDir != projPath {
		t.Fatalf("diff cwd=%q want project path %q (not process cwd)", sawDir, projPath)
	}
}

func TestSessionDiffDiscoversWorktreeWhenSessionMetadataEmpty(t *testing.T) {
	srv := workflowServer(t)
	// Corrupted/minimal session (issues only) — no project/cwd. Real bug path:
	// empty cwd used to make git run in the bot process directory.
	if err := srv.sessions.Set("1524411722717335604", sessionstore.Entry{
		Issues: []sessionstore.TrackedIssue{{Number: 514}},
	}); err != nil {
		t.Fatal(err)
	}
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	// Plant a real worktree under dataDir for this thread id under project "proj".
	wt := filepath.Join(srv.cfg.DataDir, "worktrees", "proj", "1524411722717335604")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	// Linked worktree from proj checkout.
	cmd := exec.Command("git", "-C", projPath, "worktree", "add", "-b", "grok/discord/1524411722717335604", wt)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	var sawDir string
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			sawDir = dir
			return []byte("diff --git a/proj-only.go b/proj-only.go\n--- a/proj-only.go\n+++ b/proj-only.go\n@@ -1 +1 @@\n-a\n+b\n"), nil
		}
		return orig(ctx, dir, name, args...)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/1524411722717335604/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "proj-only.go") {
		t.Fatalf("want project diff body, got %s", body)
	}
	if sawDir != wt {
		t.Fatalf("diff cwd=%q want worktree %q", sawDir, wt)
	}
	if !strings.Contains(body, "proj") {
		t.Fatalf("want recovered project name in page: %s", body)
	}
}

func TestSessionDiffEmptyCwdDoesNotUseProcessDir(t *testing.T) {
	srv := workflowServer(t)
	if err := srv.sessions.Set("orphan", sessionstore.Entry{}); err != nil {
		t.Fatal(err)
	}
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatalf("git must not run with empty/missing worktree; dir=%q args=%v", dir, args)
		return nil, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/orphan/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "no git worktree found") {
		t.Fatalf("want error about missing worktree, got %s", body)
	}
}

func execGitInit(t *testing.T, dir string) error {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := run("init"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		return err
	}
	if err := run("add", "README"); err != nil {
		return err
	}
	return run("commit", "-m", "init")
}

func TestShipBoardLinksToPRDetail(t *testing.T) {
	srv, _, _ := testServer(t)
	_ = srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "s", Project: "proj",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/3", Number: 3, State: "OPEN",
			Title: "t", Owner: "acme", Repo: "app",
		}},
	})
	req := httptest.NewRequest(http.MethodGet, "/ship", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `/prs/acme/app/3`) {
		t.Fatalf("ship missing in-app PR link: %s", body)
	}
}

func TestCommitsListAndDetail(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/projects/proj/commits?owner=acme&repo=app", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-commits-list"`,
		"acme/app",
		"Fixture commit",
		"abcdef0",
		`name="repo_full"`,
		`class="active">Commits</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %q in %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/projects/proj/commits/abcdef0?owner=acme&repo=app", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	body = w.Body.String()
	for _, want := range []string{
		`id="page-commit-detail"`,
		"Fixture commit",
		"body note",
		"foo.go",
		`href === "/commits"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
	assertNavActive(t, body, "Commits")
}

func TestCommitsIndex(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/commits", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-commits"`) || !strings.Contains(body, "/projects/proj/commits") {
		t.Fatalf("body=%s", body)
	}
	assertNavActive(t, body, "Commits")
}

func TestIssuesIndexNav(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/issues", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-issues"`) || !strings.Contains(body, "/projects/proj/issues") {
		t.Fatalf("body=%s", body)
	}
	// Nav has Issues
	if !strings.Contains(body, `href="/issues"`) {
		t.Fatal("nav missing Issues")
	}
}
