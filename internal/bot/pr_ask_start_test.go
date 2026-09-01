package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func prAskOpts() PRAskOpts {
	return PRAskOpts{
		Project:  "app",
		Actor:    Actor{ID: "u1", DisplayName: "Alice"},
		Owner:    "acme",
		Repo:     "app",
		Number:   9,
		Title:    "add retry",
		URL:      "https://github.com/acme/app/pull/9",
		State:    "OPEN",
		HeadSHA:  "abcdef0123456789abcdef0123456789abcdef01",
		HeadRef:  "feat/retry",
		BaseRef:  "main",
		Question: "Does retry cover 429s?",
	}
}

func TestBuildPRAskPromptContract(t *testing.T) {
	o := prAskOpts()
	o.Body = "why this change"
	o.Author = "dev"
	o.Additions, o.Deletions, o.ChangedFiles = 40, 7, 3
	o.Diff = "diff --git a/retry.go b/retry.go\n"
	p := BuildPRAskPrompt(o)
	for _, want := range []string{
		"Alice",
		"acme/app",
		"#9",
		"add retry",
		"Does retry cover 429s?",
		"why this change",
		"feat/retry → main",
		"abcdef0123456789abcdef0123456789abcdef01",
		"+40 −7 across 3 files",
		"https://github.com/acme/app/pull/9",
		"diff --git a/retry.go",
		"Read-only Q&A",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
	for _, banned := range []string{
		"post your findings as **one comment",
		"gh pr comment",
		"open GitHub issues",
	} {
		if strings.Contains(p, banned) && !strings.Contains(p, "Do not") {
			t.Fatalf("ask prompt must not instruct %q", banned)
		}
	}
	if !strings.Contains(p, "Do not post a GitHub comment") {
		t.Fatal("missing GitHub-write prohibition")
	}
}

func TestPRAskQuestionFromPrompt(t *testing.T) {
	p := BuildPRAskPrompt(prAskOpts())
	got := prAskQuestionFromPrompt(p)
	if got != "Does retry cover 429s?" {
		t.Fatalf("got %q", got)
	}
	if prAskQuestionFromPrompt("bare") != "bare" {
		t.Fatal("non-ask prompt should pass through")
	}
}

func TestStartPRAskCreatesHiddenUnit(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	var kind Kind
	b.startTaskHook = func(opts StartTaskOpts) { kind = opts.Kind }

	res, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if kind != KindStartInvestigate {
		t.Fatalf("kind=%v want KindStartInvestigate", kind)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("missing entry")
	}
	if !e.IsPRAsk() {
		t.Fatalf("kind=%q", e.SessionKind)
	}
	if e.AskPRKey != "acme/app#9" {
		t.Fatalf("AskPRKey=%q", e.AskPRKey)
	}
	e.NormalizePRs()
	if len(e.PRs) != 0 {
		t.Fatalf("must not bind PRs[], got %+v", e.PRs)
	}
	if e.WorktreeBranch != "" {
		t.Fatalf("WorktreeBranch=%q want empty", e.WorktreeBranch)
	}
	if hits := b.FindByPR("app", "acme", "app", 9, true); len(hits) != 0 {
		t.Fatalf("FindByPR: %+v", hits)
	}
	if listed := b.FindPRSessions("app", "acme", "app", 9); len(listed) != 0 {
		t.Fatalf("FindPRSessions: %+v", listed)
	}
	if got := b.FindPRAsk("app", "acme/app#9", "u1"); got != res.ThreadID {
		t.Fatalf("FindPRAsk=%q", got)
	}
	waitHistory(t, b, res.ThreadID, 1)
	th, err := b.history.Get(res.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) == 0 || th.Turns[0].Prompt != "Does retry cover 429s?" {
		t.Fatalf("history must record the question, not the stuffed prompt: %+v", th.Turns)
	}
}

func TestStartPRAskReusesPerViewer(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	first, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	waitHistory(t, b, first.ThreadID, 1)

	second, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ThreadID != first.ThreadID {
		t.Fatalf("want reuse, first=%+v second=%+v", first, second)
	}

	other := prAskOpts()
	other.Actor = Actor{ID: "u2", DisplayName: "Bob"}
	third, err := b.StartPRAsk(other)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Created || third.ThreadID == first.ThreadID {
		t.Fatalf("other viewer must get a new unit: %+v", third)
	}
}

func TestStartPRAskStampsReviewModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	res, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5" {
		t.Fatalf("want review model stamped when askModel is empty, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRAskStampsAskModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5", AskModel: "grok-4.6",
	})
	res, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "grok" || e.Model != "grok-4.6" {
		t.Fatalf("want ask model stamped, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRAskExplicitModelBeatsAskModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5", AskModel: "grok-4.6",
	})
	o := prAskOpts()
	o.Model = "claude-opus-5"
	res, err := b.StartPRAsk(o)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5" {
		t.Fatalf("want explicit pick stamped, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRAskReuseKeepsStamp(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5", AskModel: "grok-4.6",
	})
	first, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	waitHistory(t, b, first.ThreadID, 1)
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5", AskModel: "claude-opus-5",
	})
	second, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ThreadID != first.ThreadID {
		t.Fatalf("want reuse, first=%+v second=%+v", first, second)
	}
	e, ok := b.sessions.Get(second.ThreadID)
	if !ok || e.Agent != "grok" || e.Model != "grok-4.6" {
		t.Fatalf("reuse must keep the stamped ask model, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRAskRejectsUnknownModel(t *testing.T) {
	b, _ := testFixBot(t)
	o := prAskOpts()
	o.Model = "gpt-9"
	if _, err := b.StartPRAsk(o); err == nil || !strings.Contains(err.Error(), "gpt-9") {
		t.Fatalf("want unknown-model error naming the model, got %v", err)
	}
}

func TestStartPRAskEmptyQuestion(t *testing.T) {
	b, _ := testFixBot(t)
	o := prAskOpts()
	o.Question = "  "
	if _, err := b.StartPRAsk(o); err == nil {
		t.Fatal("expected empty prompt error")
	}
}

func TestStartContinueRefusesPRAsk(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	res, err := b.StartPRAsk(prAskOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.StartContinue(ContinueOpts{
		ThreadID: res.ThreadID, Prompt: "ship it", Actor: Actor{ID: "u1"},
	})
	if err == nil || !strings.Contains(err.Error(), "throwaway PR ask") {
		t.Fatalf("err=%v", err)
	}
}

func TestSweepPRAskDeletesIdle(t *testing.T) {
	b, _ := testFixBot(t)
	e := sessionstore.Entry{
		Project:     "app",
		SessionKind: sessionstore.SessionKindPRAsk,
		AskPRKey:    "acme/app#9",
		UpdatedAt:   time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := b.sessions.Set("w_ask_idle", e); err != nil {
		t.Fatal(err)
	}
	if err := b.history.Append("w_ask_idle", history.Turn{Prompt: "q", Response: "a", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	n := b.sweepPRAskUnits()
	if n != 1 {
		t.Fatalf("swept %d", n)
	}
	if _, ok := b.sessions.Get("w_ask_idle"); ok {
		t.Fatal("entry still present")
	}
	th, err := b.history.Get("w_ask_idle")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 0 {
		t.Fatalf("history turns=%d want deleted", len(th.Turns))
	}
}
