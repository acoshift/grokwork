package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		case strings.HasPrefix(joined, "issue list"):
			if !strings.Contains(joined, "--repo acme/api") && !strings.Contains(joined, "--repo acme/app") {
				t.Fatalf("issue list missing --repo: %v", args)
			}
			repo := "app"
			if strings.Contains(joined, "acme/api") {
				repo = "api"
			}
			return []byte(`[
				{"number":7,"url":"https://github.com/acme/` + repo + `/issues/7","title":"Fixture bug ` + repo + `","state":"OPEN","author":{"login":"alice"},"labels":[],"body":"body"}
			]`), nil
		case strings.Contains(joined, "issue view 7"):
			return []byte(`{
				"number":7,"url":"https://github.com/acme/app/issues/7","title":"Fixture bug app",
				"state":"OPEN","author":{"login":"alice"},"labels":[{"name":"bug"}],
				"body":"detail body","comments":[{"author":{"login":"bob"},"body":"note","url":"u"}]
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
	for _, want := range []string{`id="page-issue-detail"`, "Fixture bug app", "detail body", "bob", "note"} {
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
