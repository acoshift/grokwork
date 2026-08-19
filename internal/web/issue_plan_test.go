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

// Plan this feature feeds the shared confirm modal: a hidden select the modal
// clones from, and data-confirm-select so the pick is written back before submit.
// Default names the review model (resolveDispatchCLI → ReviewAgentCLI), not Fix's
// task model — the two cards share the page and must not share a label.
func TestIssueDetailShowsPlanModelConfirm(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/projects/proj/issues/42?owner=acme&repo=app")
	for _, want := range []string{
		`id="btn-plan-feature"`,
		`>Plan this feature</button>`,
		`action="/projects/proj/issues/42/plan"`,
		`data-confirm-title="Plan this feature"`,
		`data-confirm-select="model"`,
		`data-confirm-select-label="Model"`,
		`<option value="">Default (claude-opus-5)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in Plan UI", want)
		}
	}
	// Two hidden selects: Fix (task default) and Plan (review default).
	if got := strings.Count(body, `<select name="model" hidden>`); got < 2 {
		t.Fatalf("Plan form needs its own hidden select, got %d", got)
	}
	if !strings.Contains(body, `<option value="">Default (grok-4.5)</option>`) {
		t.Fatal("Fix modal Default must still name the task model")
	}
}

func TestIssueDetailReplanShowsModelConfirm(t *testing.T) {
	srv, cfg, _ := fixEnabledServer(t)
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	orig := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "issue view") {
			return []byte(`{
				"number":42,"url":"https://github.com/acme/app/issues/42","title":"Auth SSO",
				"state":"OPEN","author":{"login":"a"},"labels":[],
				"body":"## Breakdown\n- [ ] one item\n","comments":[]
			}`), nil
		}
		return orig(ctx, dir, name, args...)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/projects/proj/issues/42?owner=acme&repo=app")
	for _, want := range []string{
		`id="btn-plan-feature"`,
		`>Re-plan</button>`,
		`data-confirm-title="Re-plan feature"`,
		`data-confirm-select="model"`,
		`<option value="">Default (claude-opus-5)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in Re-plan UI", want)
		}
	}
}

func TestIssuePlanModelPickStampsSession(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "grok-4.5",
	})
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/issues/42/plan", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "model": {"claude-opus-5"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location=%q", loc)
	}
	tid := strings.TrimPrefix(strings.Split(loc, "?")[0], "/sessions/")
	bot.WaitIdleForTest(b, 5*time.Second)
	e, ok := srv.sessions.Get(tid)
	if !ok {
		t.Fatalf("session %s missing", tid)
	}
	if e.Model != "claude-opus-5" || e.Agent != "claude" {
		t.Fatalf("stamp agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestIssuePlanModelGateForInvestigator(t *testing.T) {
	srv, cfg, b := fixEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5",
	})
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/projects/proj/issues/42?owner=acme&repo=app")
	if !strings.Contains(body, `id="btn-plan-feature"`) {
		t.Fatal("investigator may still Plan (default model)")
	}
	if strings.Contains(body, `data-confirm-title="Plan this feature"`) {
		t.Fatal("investigator must not see the Plan model modal")
	}
	if strings.Count(body, `name="model"`) != 0 {
		t.Fatal("investigator must not see a model field")
	}
	w := postFix(t, srv, "/projects/proj/issues/42/plan", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "model": {"claude-opus-5"},
	})
	assertRedirectErr(t, w, "/projects/proj/issues/42", "not allowed to pick a model")
}
