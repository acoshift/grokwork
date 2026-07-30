package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestIssuePlanDispatch(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })

	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			return []byte(`{
				"number":42,"url":"https://github.com/acme/app/issues/42","title":"Auth SSO",
				"state":"OPEN","author":{"login":"a"},"labels":[],
				"body":"Users need SSO","comments":[]
			}`), nil
		}
		return orig(ctx, dir, name, args...)
	}

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/plan", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s loc=%s", w.Code, w.Body.String(), w.Header().Get("Location"))
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("want redirect to /sessions/, got %q", loc)
	}
	path := strings.SplitN(strings.TrimPrefix(loc, "/sessions/"), "?", 2)[0]
	e, ok := srv.sessions.Get(path)
	if !ok {
		t.Fatalf("session %q missing", path)
	}
	if !strings.HasPrefix(e.Goal, "Plan ") {
		t.Fatalf("goal=%q", e.Goal)
	}
	// Planning unit must not bind the issue.
	if len(e.Issues) != 0 {
		t.Fatalf("plan must not bind issue: %+v", e.Issues)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
}

func TestIssueItemStartHappyPath(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	cfg.WebPublicBaseURL = "https://gw.example"

	const raw = "- [ ] Add OIDC callback"
	const issueBody = "## Breakdown\n<!-- grokwork:tasklist -->\n" + raw + "\n- [ ] other item\n"
	var editedBody string
	var sawEdit bool

	runner := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			payload, _ := json.Marshal(map[string]any{
				"number":   42,
				"url":      "https://github.com/acme/app/issues/42",
				"title":    "Auth SSO",
				"state":    "OPEN",
				"author":   map[string]string{"login": "a"},
				"labels":   []any{},
				"body":     issueBody,
				"comments": []any{},
			})
			return payload, nil
		}
		if strings.Contains(joined, "issue edit") {
			sawEdit = true
			for i, a := range args {
				if a == "--body-file" && i+1 < len(args) {
					bb, err := os.ReadFile(args[i+1])
					if err != nil {
						t.Fatal(err)
					}
					editedBody = string(bb)
				}
			}
			return []byte("ok"), nil
		}
		return []byte("{}"), nil
	}
	srv.ghRunner = runner
	// The annotation edit runs bot-side; install the same runner there (at test
	// setup, never on the request path — SetGHRunner is not request-safe).
	b.SetGHRunner(runner)

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/items/start", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "raw_line": {raw},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s loc=%s", w.Code, w.Body.String(), w.Header().Get("Location"))
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("loc=%q", loc)
	}
	threadID := strings.SplitN(strings.TrimPrefix(loc, "/sessions/"), "?", 2)[0]
	e, ok := srv.sessions.Get(threadID)
	if !ok {
		t.Fatal("missing session")
	}
	if e.Goal != "Add OIDC callback" {
		t.Fatalf("goal=%q", e.Goal)
	}
	if len(e.Issues) != 1 || e.Issues[0].Number != 42 {
		t.Fatalf("issues=%+v", e.Issues)
	}
	if e.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordRefs {
		t.Fatalf("keyword=%q want Refs", e.Issues[0].EffectiveKeyword())
	}
	if !sawEdit {
		t.Fatal("expected issue edit annotation")
	}
	if !strings.Contains(editedBody, "/sessions/"+threadID) {
		t.Fatalf("annotated body missing session url: %q", editedBody)
	}
	if !strings.Contains(editedBody, raw+" — [session](") {
		t.Fatalf("annotation not on line: %q", editedBody)
	}
	assertAuditAction(t, srv, audit.ActionSessionStart, true)
}

func TestIssueItemStartRefusesMissingRawLine(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })

	const issueBody = "## Breakdown\n- [ ] still here\n"
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue view") {
			payload, _ := json.Marshal(map[string]any{
				"number": 42, "url": "https://github.com/acme/app/issues/42",
				"title": "T", "state": "OPEN", "author": map[string]string{"login": "a"},
				"labels": []any{}, "body": issueBody, "comments": []any{},
			})
			return payload, nil
		}
		return []byte("{}"), nil
	}

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/items/start", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
		"raw_line": {"- [ ] gone from body"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("must not start session: %q", loc)
	}
	if !strings.Contains(loc, "/issues/42") || !strings.Contains(loc, "err=") {
		t.Fatalf("want issue redirect with err: %q", loc)
	}
}

func TestIssueItemStartRefusesAlreadyAnnotated(t *testing.T) {
	srv, _, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })

	const issueBody = "## Breakdown\n- [ ] already linked — [session](https://gw.example/sessions/w_old)\n"
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue view") {
			payload, _ := json.Marshal(map[string]any{
				"number": 42, "url": "https://github.com/acme/app/issues/42",
				"title": "T", "state": "OPEN", "author": map[string]string{"login": "a"},
				"labels": []any{}, "body": issueBody, "comments": []any{},
			})
			return payload, nil
		}
		t.Fatal("must not issue edit")
		return nil, nil
	}

	items := bot.ParseTasklist(issueBody)
	if len(items) != 1 || items[0].SessionURL == "" {
		t.Fatalf("%+v", items)
	}

	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/items/start", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"},
		"raw_line": {items[0].RawLine},
	})
	loc := w.Header().Get("Location")
	if strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("must refuse already-started item: %q", loc)
	}
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want err flash: %q", loc)
	}
}

func TestIssueDetailRendersBreakdown(t *testing.T) {
	srv := issueDetailWithSessions(t)
	const body = "## Breakdown\n- [x] done one\n- [ ] open two\n"
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue view 42") {
			payload, _ := json.Marshal(map[string]any{
				"number": 42, "url": "https://github.com/acme/app/issues/42",
				"title": "Feature hub", "state": "OPEN",
				"author": map[string]string{"login": "alice"},
				"labels": []any{}, "body": body, "comments": []any{},
			})
			return payload, nil
		}
		return orig(ctx, dir, name, args...)
	}
	// Auth-off workflow server: FeatureStartSessions is true when web auth is off
	// (featureFlag fails open). Breakdown renders regardless of CanPlanFeature.

	req := httptest.NewRequest(http.MethodGet, "/projects/proj/issues/42?owner=acme&repo=app", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	html := w.Body.String()
	if !strings.Contains(html, `id="issue-breakdown"`) {
		t.Fatal("missing issue-breakdown")
	}
	if !strings.Contains(html, "1/2") {
		t.Fatalf("want progress 1/2; snippet: %s", bodySnippet(html, "issue-breakdown", 200))
	}
	if !strings.Contains(html, "done one") || !strings.Contains(html, "open two") {
		t.Fatal("missing item text")
	}
}
