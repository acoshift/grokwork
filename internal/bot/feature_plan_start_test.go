package bot

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func featurePlanOpts() FeaturePlanOpts {
	return FeaturePlanOpts{
		Project: "app",
		Actor:   Actor{ID: "u1", DisplayName: "Alice"},
		Owner:   "acme",
		Repo:    "app",
		Number:  42,
		Title:   "Auth SSO",
		URL:     "https://github.com/acme/app/issues/42",
		Body:    "Users need single sign-on.",
	}
}

func TestBuildFeaturePlanPromptContract(t *testing.T) {
	p := BuildFeaturePlanPrompt(featurePlanOpts())
	for _, want := range []string{
		"Alice",
		"acme/app",
		"#42",
		"Auth SSO",
		"Users need single sign-on.",
		"https://github.com/acme/app/issues/42",
		"gh issue view 42 --repo acme/app",
		"gh issue edit 42 --repo acme/app --body-file",
		"## Breakdown",
		"<!-- grokwork:tasklist -->",
		"Plan only",
		"Do not implement anything",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestStartFeaturePlanNeverOpensDiscordThread(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextMsg: "m1", nextTh: "th-plan-1"}
	b.threadAPI = fake

	res, err := b.StartFeaturePlan(featurePlanOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if len(fake.starts) != 0 || len(fake.sends) != 0 {
		t.Fatalf("thread API must be untouched: starts=%v sends=%v", fake.starts, fake.sends)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Origin != SourceWeb || e.CreatedBy != "u1" {
		t.Fatalf("%+v", e)
	}
	if !strings.HasPrefix(e.Goal, "Plan ") || !strings.Contains(e.Goal, "acme/app#42") {
		t.Fatalf("goal=%q", e.Goal)
	}
	// Planning unit must not bind the issue (would pollute Fix reuse picker).
	if len(e.Issues) != 0 {
		t.Fatalf("plan unit must not bind the issue, got %+v", e.Issues)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartFeaturePlanDoesNotBindIssue(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	res, err := b.StartFeaturePlan(featurePlanOpts())
	if err != nil {
		t.Fatal(err)
	}
	if hits := b.FindByIssue("app", "acme", "app", 42, true); len(hits) != 0 {
		t.Fatalf("plan unit must not appear in FindByIssue, got %+v", hits)
	}
	_ = res
}

func TestBuildChecklistItemPromptContract(t *testing.T) {
	p := BuildChecklistItemPrompt(ChecklistItemOpts{
		Actor:      Actor{DisplayName: "Bob"},
		Owner:      "acme",
		Repo:       "app",
		Number:     42,
		IssueTitle: "Auth SSO",
		IssueURL:   "https://github.com/acme/app/issues/42",
		ItemText:   "Add OIDC callback route",
	})
	for _, want := range []string{
		"Bob",
		"acme/app#42",
		"Auth SSO",
		"Add OIDC callback route",
		"Your scope is exactly this sub-task",
		"Refs acme/app#42",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestStartChecklistItemBindsRefsAndAnnotates(t *testing.T) {
	b, proj := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	b.cfg.WebPublicBaseURL = "https://gw.example"

	const raw = "- [ ] Add OIDC callback route"
	const body = "## Breakdown\n" + raw + "\n- [ ] other\n"
	var editedBody string
	var sawEdit bool
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "issue view") {
			payload, _ := json.Marshal(map[string]any{
				"number":   42,
				"url":      "https://github.com/acme/app/issues/42",
				"title":    "Auth SSO",
				"state":    "OPEN",
				"author":   map[string]string{"login": "a"},
				"labels":   []any{},
				"body":     body,
				"comments": []any{},
			})
			return payload, nil
		}
		if strings.Contains(joined, "issue edit") {
			sawEdit = true
			for i, a := range args {
				if a == "--body-file" && i+1 < len(args) {
					b, err := os.ReadFile(args[i+1])
					if err != nil {
						t.Fatal(err)
					}
					editedBody = string(b)
				}
			}
			return []byte("ok"), nil
		}
		return []byte("{}"), nil
	})

	res, err := b.StartChecklistItem(ChecklistItemOpts{
		Project:    "app",
		Owner:      "acme",
		Repo:       "app",
		Number:     42,
		IssueTitle: "Auth SSO",
		IssueURL:   "https://github.com/acme/app/issues/42",
		ItemText:   "Add OIDC callback route",
		RawLine:    raw,
		Actor:      Actor{ID: "u1", DisplayName: "Alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("%+v", res)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("missing session")
	}
	if e.Goal != "Add OIDC callback route" {
		t.Fatalf("goal=%q", e.Goal)
	}
	if len(e.Issues) != 1 || e.Issues[0].Number != 42 || e.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordRefs {
		t.Fatalf("issues=%+v", e.Issues)
	}
	if !sawEdit {
		t.Fatal("expected issue edit for annotation")
	}
	if !strings.Contains(editedBody, "/sessions/"+res.ThreadID) {
		t.Fatalf("edited body missing session url: %q", editedBody)
	}
	if !strings.Contains(editedBody, raw+" — [session](") {
		t.Fatalf("annotation not on raw line: %q", editedBody)
	}
	_ = proj
}

func TestStartChecklistItemSkipsAnnotateWithoutPublicURL(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// WebPublicBaseURL empty → no annotation attempt.
	called := false
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	res, err := b.StartChecklistItem(ChecklistItemOpts{
		Project:  "app",
		Owner:    "acme",
		Repo:     "app",
		Number:   1,
		ItemText: "do thing",
		RawLine:  "- [ ] do thing",
		Actor:    Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("gh must not run when WebPublicBaseURL is empty")
	}
	_ = res
}

func TestStartFeaturePlanStampsReviewModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	res, err := b.StartFeaturePlan(featurePlanOpts())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5" {
		t.Fatalf("want review model stamped, got agent=%q model=%q", e.Agent, e.Model)
	}
}
