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
		case name == "git" && len(args) > 0 && args[0] == "diff" && strings.Contains(joined, "--numstat"):
			return []byte("1\t1\twt.go\x00"), nil
		case name == "git" && len(args) > 0 && args[0] == "diff" && strings.Contains(joined, "--name-status"):
			return []byte("M\x00wt.go\x00"), nil
		case name == "git" && len(args) > 0 && args[0] == "diff":
			return []byte("diff --git a/wt.go b/wt.go\n--- a/wt.go\n+++ b/wt.go\n@@ -1 +1 @@\n-a\n+b\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "merge-base":
			// Session worktree diff resolves merge-base(base, HEAD) before diff.
			return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "rev-list":
			// DetectClosestBaseRef scores candidates by ahead-count.
			return []byte("1\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "log":
			return []byte("abcdef0123456789\x1fFixture commit\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "rev-parse":
			// DetectClosestBaseRef / PreferOriginRef use --verify --quiet.
			// Only claim origin/main exists so default session base stays stable.
			// Other rev-parse (--verify SHA, etc.) still succeeds for commit pages.
			if strings.Contains(joined, "--verify") && strings.Contains(joined, "--quiet") {
				ref := args[len(args)-1]
				if ref == "origin/main" {
					return []byte(ref + "\n"), nil
				}
				return nil, fmt.Errorf("unknown ref %s", ref)
			}
			return []byte("abcdef0123456789abcdef0123456789abcdef01\n"), nil
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "--numstat"):
			return []byte("1\t1\tfoo.go\x00"), nil
		case name == "git" && len(args) > 0 && args[0] == "show" && strings.Contains(joined, "--name-status"):
			return []byte("M\x00foo.go\x00"), nil
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
	// History-restore sync must re-derive the workspace scope from the URL.
	if !strings.Contains(body, "function navActiveFor") || !strings.Contains(body, "function scopeFromLocation") {
		t.Fatal("layout missing navActiveFor/scopeFromLocation for workspace URLs")
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
	// Workspace nav gives Linear its own tab when enabled for the project.
	assertNavActive(t, body, "Linear")
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
	assertNavActive(t, body, "Linear")
}

func TestPRDetailAndDiff(t *testing.T) {
	srv := workflowServer(t)
	var prDiffCalls int
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "diff" {
			prDiffCalls++
		}
		return orig(ctx, dir, name, args...)
	}
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
	// Index-only page: files listed as lazy cards, hunks live in fragments.
	for _, want := range []string{
		`id="page-diff"`,
		`id="diff-review"`,
		`data-review-key="pr:acme/app#9"`,
		`data-path="foo.go"`,
		`hx-get="/prs/acme/app/9/diff/file?path=foo.go&amp;project=proj"`,
		`hx-trigger="intersect once"`,
		"1 files changed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("diff missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "@@") {
		t.Fatal("diff page must not inline hunks (fragments only)")
	}

	// Per-file fragment carries the hunks with line numbers.
	req = httptest.NewRequest(http.MethodGet, "/prs/acme/app/9/diff/file?path=foo.go&project=proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("frag status=%d body=%s", w.Code, w.Body.String())
	}
	body = w.Body.String()
	for _, want := range []string{`class="dpatch"`, "@@ -1 &#43;1 @@", `class="dl del"`, `class="dl add"`, "-old", "&#43;new"} {
		if !strings.Contains(body, want) {
			t.Fatalf("frag missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "<nav") || strings.Contains(body, "id=\"live-root\"") {
		t.Fatal("fragment must not contain layout chrome")
	}

	// Path traversal is rejected.
	req = httptest.NewRequest(http.MethodGet, "/prs/acme/app/9/diff/file?path=../secrets&project=proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal status=%d want 400", w.Code)
	}

	// Page + fragment share one cached gh pr diff fetch.
	if prDiffCalls != 1 {
		t.Fatalf("gh pr diff calls = %d, want 1 (patch cache)", prDiffCalls)
	}
}

func TestSessionDiff(t *testing.T) {
	srv := workflowServer(t)
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path missing")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(srv.cfg.DataDir, "worktrees", "proj", "thread-99")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", projPath, "worktree", "add", "-b", "grok/discord/thread-99", wt)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "s", Project: "proj", Cwd: wt, MainCwd: projPath,
		WorktreeBranch: "grok/discord/thread-99",
	}); err != nil {
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
	if !strings.Contains(body, "wt.go") || !strings.Contains(body, `id="diff-review"`) {
		t.Fatalf("body=%s", body)
	}
	if !strings.Contains(body, `hx-get="/sessions/thread-99/diff/file?base=origin%2Fmain&amp;path=wt.go"`) {
		t.Fatalf("missing per-file fragment URL in %s", body)
	}
	if sawDir != wt {
		t.Fatalf("diff cwd=%q want worktree %q", sawDir, wt)
	}

	// Fragment endpoint renders the hunks for one file.
	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99/diff/file?base=origin%2Fmain&path=wt.go", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("frag status=%d body=%s", w.Code, w.Body.String())
	}
	body = w.Body.String()
	if !strings.Contains(body, `class="dpatch"`) || !strings.Contains(body, "&#43;b") {
		t.Fatalf("frag body=%s", body)
	}
}

func TestSessionDiffMissingWorktreeShowsError(t *testing.T) {
	// Session still tracked, but worktree was pruned — must not fall back to main checkout.
	srv := workflowServer(t)
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("gone-wt", sessionstore.Entry{
		SessionID: "s", Project: "proj",
		Cwd:            filepath.Join(srv.cfg.DataDir, "worktrees", "proj", "gone-wt"),
		MainCwd:        projPath,
		WorktreeBranch: "grok/discord/gone-wt",
	}); err != nil {
		t.Fatal(err)
	}
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatalf("git must not run when worktree is gone; dir=%q args=%v", dir, args)
		return nil, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/gone-wt/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "worktree no longer on disk") {
		t.Fatalf("want missing-worktree error, got %s", body)
	}
	if strings.Contains(body, `id="diff-review"`) && strings.Contains(body, "wt.go") {
		t.Fatal("must not render a main-checkout diff when worktree is gone")
	}

	// Session page: Worktree diff link is present but marked disabled/muted.
	req = httptest.NewRequest(http.MethodGet, "/sessions/gone-wt", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session status=%d", w.Code)
	}
	sess := w.Body.String()
	// Match the anchor itself (layout CSS also mentions aria-disabled).
	if !strings.Contains(sess, `href="/sessions/gone-wt/diff?project=proj" class="muted" aria-disabled="true" title="Worktree no longer on disk">Worktree diff</a>`) {
		t.Fatalf("want disabled Worktree diff link, got %s", sess)
	}
}

func TestSessionDiffIsolationOffUsesMainCwd(t *testing.T) {
	// worktreeIsolation=false: session cwd is the main checkout — still a valid diff root.
	srv := workflowServer(t)
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("main-cwd", sessionstore.Entry{
		SessionID: "s", Project: "proj", Cwd: projPath, MainCwd: projPath,
	}); err != nil {
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
	req := httptest.NewRequest(http.MethodGet, "/sessions/main-cwd/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if sawDir != projPath {
		t.Fatalf("diff cwd=%q want main checkout %q", sawDir, projPath)
	}
	body := w.Body.String()
	if strings.Contains(body, "worktree no longer on disk") {
		t.Fatal("isolation-off main cwd must not be treated as missing worktree")
	}

	req = httptest.NewRequest(http.MethodGet, "/sessions/main-cwd", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	sess := w.Body.String()
	if !strings.Contains(sess, `href="/sessions/main-cwd/diff?project=proj">Worktree diff</a>`) {
		t.Fatalf("want enabled Worktree diff link, got %s", sess)
	}
	if strings.Contains(sess, `href="/sessions/main-cwd/diff?project=proj" class="muted"`) {
		t.Fatal("Worktree diff must not be muted when session cwd is the live main checkout")
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
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "--numstat") {
				return []byte("1\t1\tproj-only.go\x00"), nil
			}
			if strings.Contains(joined, "--name-status") {
				return []byte("M\x00proj-only.go\x00"), nil
			}
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
	if !strings.Contains(body, "worktree no longer on disk") {
		t.Fatalf("want error about missing worktree, got %s", body)
	}
}

func TestSessionDiffUsesPRBaseNotMain(t *testing.T) {
	// Backport-to-prod session: diff must use origin/prod, not origin/main.
	srv := workflowServer(t)
	projPath, ok := srv.cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(srv.cfg.DataDir, "worktrees", "proj", "bp-thread")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", projPath, "worktree", "add", "-b", "grok/discord/bp-thread", wt)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := srv.sessions.Set("bp-thread", sessionstore.Entry{
		SessionID: "s", Project: "proj", Cwd: wt, MainCwd: projPath,
		WorktreeBranch: "grok/discord/bp-thread",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/529", Number: 529, State: "OPEN",
			Owner: "acme", Repo: "app", Title: "[backport→prod] feature",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var mergeBaseLeft string
	var sawPRBaseJSON bool
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "gh" && strings.HasPrefix(joined, "pr view") && strings.Contains(joined, "baseRefName") {
			sawPRBaseJSON = true
			// Lightweight PRBaseRefWith call (and full view if ever used).
			if strings.Contains(joined, "number,url") {
				return []byte(`{
					"number":529,"url":"https://github.com/acme/app/pull/529","title":"bp",
					"state":"OPEN","isDraft":false,"reviewDecision":"","headRefOid":"abc",
					"headRefName":"grok/discord/bp-thread","baseRefName":"prod","body":"",
					"mergeable":"MERGEABLE","author":{"login":"z"},
					"additions":1,"deletions":0,"changedFiles":1
				}`), nil
			}
			return []byte(`{"baseRefName":"prod"}`), nil
		}
		if name == "git" && len(args) > 0 && args[0] == "rev-parse" && strings.Contains(joined, "--verify") {
			ref := args[len(args)-1]
			if ref == "origin/prod" || ref == "origin/main" {
				return []byte(ref + "\n"), nil
			}
			return nil, fmt.Errorf("unknown ref %s", ref)
		}
		if name == "git" && len(args) > 0 && args[0] == "merge-base" {
			// Record which base the worktree diff resolved (left of merge-base is the base ref).
			if len(args) >= 2 && args[1] != "HEAD" {
				// PreferOrigin / Resolve may merge-base origin/prod HEAD first.
				if args[1] == "origin/prod" || args[1] == "origin/main" {
					// fall through after note
				}
			}
			if len(args) >= 3 && args[2] == "HEAD" {
				// base is args[1]
				_ = args[1]
			}
			// Distinct merge-bases so we can see which base was chosen for the actual diff.
			if len(args) >= 2 && args[1] == "origin/prod" {
				return []byte("cccccccccccccccccccccccccccccccccccccccc\n"), nil
			}
			if len(args) >= 2 && args[1] == "origin/main" {
				return []byte("dddddddddddddddddddddddddddddddddddddddd\n"), nil
			}
			return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
		}
		if name == "git" && len(args) > 0 && args[0] == "rev-list" {
			if strings.Contains(joined, "cccc") {
				return []byte("2\n"), nil
			}
			if strings.Contains(joined, "dddd") {
				return []byte("100\n"), nil
			}
			return []byte("1\n"), nil
		}
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			// The left side of the worktree diff is the merge-base sha.
			if len(args) >= 2 {
				mergeBaseLeft = args[1]
				if args[1] == "--numstat" || args[1] == "--name-status" {
					// git diff --numstat -z LEFT
					for _, a := range args {
						if len(a) == 40 && a[0] == 'c' {
							mergeBaseLeft = a
						}
						if len(a) == 40 && (a[0] == 'c' || a[0] == 'd' || a[0] == 'a') {
							mergeBaseLeft = a
						}
					}
					// args like: diff --numstat -z cccc...
					for i, a := range args {
						if a == "-z" && i+1 < len(args) {
							mergeBaseLeft = args[i+1]
						}
					}
				}
			}
			if strings.Contains(joined, "--numstat") {
				return []byte("1\t0\timagine.go\x00"), nil
			}
			if strings.Contains(joined, "--name-status") {
				return []byte("A\x00imagine.go\x00"), nil
			}
			return []byte("diff --git a/imagine.go b/imagine.go\n--- /dev/null\n+++ b/imagine.go\n@@ -0,0 +1 @@\n+x\n"), nil
		}
		return orig(ctx, dir, name, args...)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/bp-thread/diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !sawPRBaseJSON {
		t.Fatal("expected gh pr view for baseRefName")
	}
	if !strings.Contains(body, "origin/prod") {
		t.Fatalf("page should show origin/prod base, body=%s", body)
	}
	if strings.Contains(body, "origin/main") {
		t.Fatalf("must not use origin/main for prod backport, body=%s", body)
	}
	if mergeBaseLeft != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("diff left merge-base=%q want prod's mb", mergeBaseLeft)
	}
	if !strings.Contains(body, "imagine.go") {
		t.Fatalf("want imagine.go in index: %s", body)
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
		`id="diff-review"`,
		`hx-get="/projects/proj/commits/abcdef0123456789abcdef0123456789abcdef01/file?path=foo.go"`,
		"function scopeFromLocation",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
	assertNavActive(t, body, "Commits")

	// Per-file fragment for the commit.
	req = httptest.NewRequest(http.MethodGet, "/projects/proj/commits/abcdef0/file?path=foo.go", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("frag status=%d body=%s", w.Code, w.Body.String())
	}
	body = w.Body.String()
	if !strings.Contains(body, `class="dpatch"`) || !strings.Contains(body, "&#43;new") {
		t.Fatalf("frag body=%s", body)
	}
	if !strings.Contains(body, `<span class="ln">1</span>`) {
		t.Fatalf("frag missing line numbers: %s", body)
	}
}

// Feature-first hubs are retired: /commits and /issues redirect to the
// project launcher (projects are picked first, then the feature).
func TestCommitsIndex(t *testing.T) {
	srv := workflowServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/commits", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want redirect", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q want /", loc)
	}
}

func TestIssuesIndexNav(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/issues", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want redirect", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q want /", loc)
	}
}
