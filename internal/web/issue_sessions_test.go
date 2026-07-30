package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// issueDetailWithSessions seeds a GitHub catalog + issue-view fake and returns
// the server ready for GET /projects/proj/issues/{n}.
func issueDetailWithSessions(t *testing.T) *Server {
	t.Helper()
	srv := workflowServer(t)
	// workflowServer already fakes issue view 7; keep that and add 42 for hub tests.
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "issue view 42") {
			return []byte(`{
				"number":42,"url":"https://github.com/acme/app/issues/42","title":"Feature hub",
				"state":"OPEN","author":{"login":"alice"},"labels":[],
				"body":"scope","comments":[]
			}`), nil
		}
		return orig(ctx, dir, name, args...)
	}
	return srv
}

func TestIssueDetailSessionsSection(t *testing.T) {
	srv := issueDetailWithSessions(t)

	open := sessionstore.Entry{
		Project: "proj", Goal: "implement auth flow",
		OwnerName: "alice", Label: sessionstore.LabelInProgress,
	}
	open.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42})
	if err := srv.sessions.Set("th-open-auth", open); err != nil {
		t.Fatal(err)
	}
	// Terminal sessions are the feature's history — hub must list them too.
	done := sessionstore.Entry{
		Project: "proj", Goal: "scaffold endpoints",
		OwnerName: "bob", Label: sessionstore.LabelDone,
	}
	done.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42})
	if err := srv.sessions.Set("th-done-scaffold", done); err != nil {
		t.Fatal(err)
	}
	// Wrong issue — must not appear.
	other := sessionstore.Entry{Project: "proj", Goal: "other work"}
	other.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 99})
	if err := srv.sessions.Set("th-other", other); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/42?owner=acme&repo=app", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="issue-sessions"`) {
		t.Fatal("missing issue-sessions section")
	}
	if !strings.Contains(body, "Sessions (2)") {
		t.Fatalf("want Sessions (2) heading; body snippet around sessions: %s", bodySnippet(body, "issue-sessions", 400))
	}
	for _, want := range []string{
		"implement auth flow",
		"scaffold endpoints",
		// Provenance crumb on each row (amp-encoded in HTML).
		`back=` + url.QueryEscape("/projects/proj/issues/42?owner=acme&repo=app"),
		// Terminal label chip.
		`class="badge status-done">done</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(body, "other work") {
		t.Fatal("session for a different issue must not appear")
	}
	// Amp-encoding of the goal link (cases.tmpl pattern).
	if !strings.Contains(body, `href="/sessions/th-open-auth?project=proj&amp;back=`) {
		t.Fatalf("goal link missing amp-encoded back=; snippet: %s", bodySnippet(body, "th-open-auth", 200))
	}
}

func TestIssueDetailSessionsSectionAbsentWhenEmpty(t *testing.T) {
	srv := issueDetailWithSessions(t)
	// No session binds #42.
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/42?owner=acme&repo=app", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `id="issue-sessions"`) {
		t.Fatal("issue-sessions section must be absent when no sessions bind the issue")
	}
}

func bodySnippet(body, marker string, n int) string {
	i := strings.Index(body, marker)
	if i < 0 {
		if len(body) > n {
			return body[:n]
		}
		return body
	}
	end := min(i+n, len(body))
	return body[i:end]
}
