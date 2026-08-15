package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParsePlanIssueAndVerdict(t *testing.T) {
	text := strings.Join([]string{
		"Here is the plan.",
		"SCRUTINIZE_VERDICT: ship",
		"traced executeTask and BuildRunPolicy",
		"",
		"PLAN_ISSUE:",
		"title: Add plan mode",
		"",
		"## Context",
		"Builders need a start mode that files a plan.",
		"",
		"## Breakdown",
		"<!-- grokwork:tasklist -->",
		"- [ ] policy + prefix",
		"- [ ] file the issue",
		"",
		"SESSION_DONE:",
	}, "\n")
	spec, rem, ok := parsePlanIssue(text)
	if !ok {
		t.Fatal("expected PLAN_ISSUE")
	}
	if spec.Title != "Add plan mode" {
		t.Fatalf("title=%q", spec.Title)
	}
	if !planBodyHasTasklist(spec.Body) {
		t.Fatalf("body missing tasklist:\n%s", spec.Body)
	}
	if strings.Contains(rem, "PLAN_ISSUE:") {
		t.Fatalf("remainder still has PLAN_ISSUE:\n%s", rem)
	}
	if parseScrutinizeVerdict(rem) != "ship" {
		t.Fatalf("verdict from remainder=%q", parseScrutinizeVerdict(rem))
	}
	kind, _ := parseSessionLifecycleMarker(rem)
	if kind != sessionMarkerDone {
		t.Fatalf("marker=%v", kind)
	}
}

func TestParsePlanIssueQuotedTokensInBodyDoNotCloseEarly(t *testing.T) {
	text := "PLAN_ISSUE:\ntitle: X\n\nDo not write SESSION_DONE: in the body mid-line.\n\n## Breakdown\n- [ ] one\n"
	spec, _, ok := parsePlanIssue(text)
	if !ok {
		t.Fatal("expected parse")
	}
	if !strings.Contains(spec.Body, "SESSION_DONE:") {
		t.Fatalf("mid-line SESSION_DONE should stay in body:\n%s", spec.Body)
	}
}

func TestParsePlanIssueMissingTitleOrTasklist(t *testing.T) {
	if _, _, ok := parsePlanIssue("no block here"); ok {
		t.Fatal("want miss")
	}
	if _, _, ok := parsePlanIssue("PLAN_ISSUE:\n\njust a body\n"); ok {
		t.Fatal("want missing title")
	}
	spec, _, ok := parsePlanIssue("PLAN_ISSUE:\ntitle: Only prose\n\nNo checklist here.\n")
	if !ok {
		t.Fatal("title+body should parse")
	}
	if planBodyHasTasklist(spec.Body) {
		t.Fatal("prose is not a tasklist")
	}
}

func TestMaybeFilePlanIssuePreconditions(t *testing.T) {
	b, dir := testFixBot(t)
	thread := "w_plan_pre"
	if err := b.sessions.Set(thread, sessionstore.Entry{Project: "app", Mode: ModePlan}); err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "u", DisplayName: "U"}

	good := strings.Join([]string{
		"SCRUTINIZE_VERDICT: ship",
		"PLAN_ISSUE:",
		"title: Add plan mode",
		"",
		"## Breakdown",
		"- [ ] do it",
	}, "\n")

	out := b.maybeFilePlanIssue(thread, "app", dir, actor, good, true, false, 0)
	if out.Filed || !strings.Contains(out.Note, "cancelled") {
		t.Fatalf("cancelled: %+v", out)
	}
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, good, false, true, 0)
	if out.Filed || !strings.Contains(out.Note, "max turns") {
		t.Fatalf("max turns: %+v", out)
	}
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, good, false, false, 2)
	if out.Filed || !strings.Contains(out.Note, "exit 2") {
		t.Fatalf("exit: %+v", out)
	}
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, "no verdict or block", false, false, 0)
	if out.Filed || !strings.Contains(out.Note, "SCRUTINIZE_VERDICT") {
		t.Fatalf("no verdict: %+v", out)
	}
	rework := "SCRUTINIZE_VERDICT: rework\nPLAN_ISSUE:\ntitle: X\n\n## Breakdown\n- [ ] a\n"
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, rework, false, false, 0)
	if out.Filed || !strings.Contains(out.Note, "rework") {
		t.Fatalf("rework: %+v", out)
	}
	noList := "SCRUTINIZE_VERDICT: ship\nPLAN_ISSUE:\ntitle: X\n\njust words\n"
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, noList, false, false, 0)
	if out.Filed || !strings.Contains(out.Note, "tasklist") {
		t.Fatalf("no tasklist: %+v", out)
	}

	if _, _, err := b.sessions.Patch(thread, func(e *sessionstore.Entry) {
		e.OpenQuestions = []sessionstore.OpenQuestion{{ID: "q1", Text: "?", Status: "open"}}
	}); err != nil {
		t.Fatal(err)
	}
	out = b.maybeFilePlanIssue(thread, "app", dir, actor, good, false, false, 0)
	if out.Filed || !strings.Contains(out.Note, "open questions") {
		t.Fatalf("open Q: %+v", out)
	}
}

func TestMaybeFilePlanIssueCreatesAndUpdates(t *testing.T) {
	b, dir := testFixBot(t)
	var creates, edits int
	b.SetGHRunner(func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "issue create") {
			creates++
			if !strings.Contains(joined, "--label") || !strings.Contains(joined, planIssueLabel) {
				t.Fatalf("create args=%v", args)
			}
			return []byte("https://github.com/acme/app/issues/9\n"), nil
		}
		if strings.Contains(joined, "issue edit") {
			edits++
			return []byte(""), nil
		}
		if strings.Contains(joined, "label create") {
			t.Fatalf("label should already exist in this test: %v", args)
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	})
	thread := "w_plan_file"
	if err := b.sessions.Set(thread, sessionstore.Entry{Project: "app", Mode: ModePlan}); err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "u", DisplayName: "U"}
	good := strings.Join([]string{
		"SCRUTINIZE_VERDICT: ship",
		"PLAN_ISSUE:",
		"title: Add plan mode",
		"",
		"## Breakdown",
		"- [ ] policy",
	}, "\n")
	out := b.maybeFilePlanIssue(thread, "app", dir, actor, good, false, false, 0)
	if !out.Filed || out.Updated || creates != 1 {
		t.Fatalf("create: %+v creates=%d", out, creates)
	}
	e, ok := b.sessions.Get(thread)
	if !ok || len(e.Issues) != 1 || e.Issues[0].Number != 9 {
		t.Fatalf("bound=%+v", e.Issues)
	}
	if e.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordRefs {
		t.Fatalf("keyword=%q", e.Issues[0].EffectiveKeyword())
	}

	out = b.maybeFilePlanIssue(thread, "app", dir, actor, good, false, false, 0)
	if !out.Filed || !out.Updated || creates != 1 || edits != 1 {
		t.Fatalf("update: %+v creates=%d edits=%d", out, creates, edits)
	}
}

func TestMaybeFilePlanIssueCreatesMissingLabel(t *testing.T) {
	b, dir := testFixBot(t)
	var createdLabel bool
	b.SetGHRunner(func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "issue create") && strings.Contains(joined, "--label") && !createdLabel:
			return nil, &plainError{"could not add label: 'plan' not found"}
		case strings.Contains(joined, "label create"):
			createdLabel = true
			return []byte(""), nil
		case strings.Contains(joined, "issue create"):
			return []byte("https://github.com/acme/app/issues/3\n"), nil
		default:
			t.Fatalf("unexpected %v", args)
			return nil, nil
		}
	})
	thread := "w_plan_lab"
	if err := b.sessions.Set(thread, sessionstore.Entry{Project: "app", Mode: ModePlan}); err != nil {
		t.Fatal(err)
	}
	good := "SCRUTINIZE_VERDICT: ship\nPLAN_ISSUE:\ntitle: X\n\n## Breakdown\n- [ ] a\n"
	out := b.maybeFilePlanIssue(thread, "app", dir, Actor{ID: "u"}, good, false, false, 0)
	if !out.Filed || !createdLabel || out.Issue.Number != 3 {
		t.Fatalf("out=%+v createdLabel=%v", out, createdLabel)
	}
}

func TestStoreOpenQuestionsWithoutDiscord(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.sessions.Set("w_q", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	n := b.storeOpenQuestions("w_q", []decisionSpec{{
		ID: "q1", Prompt: "Which package?", Options: []string{"api", "new"},
	}})
	if n != 1 {
		t.Fatalf("stored=%d", n)
	}
	e, _ := b.sessions.Get("w_q")
	if len(e.OpenQuestions) != 1 || e.OpenQuestions[0].ID != "q1" || e.OpenQuestions[0].Status != "open" {
		t.Fatalf("%+v", e.OpenQuestions)
	}
}

func TestPlanSessionDoneAllowed(t *testing.T) {
	if planSessionDoneAllowed(planFileOutcome{}) {
		t.Fatal("empty not allowed")
	}
	if !planSessionDoneAllowed(planFileOutcome{Filed: true}) {
		t.Fatal("filed allowed")
	}
}

type plainError struct{ s string }

func (e *plainError) Error() string { return e.s }
