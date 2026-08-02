package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func prReviewOpts() PRReviewOpts {
	return PRReviewOpts{
		Project: "app",
		Actor:   Actor{ID: "u1", DisplayName: "Alice"},
		Owner:   "acme",
		Repo:    "app",
		Number:  9,
		Title:   "add retry",
		URL:     "https://github.com/acme/app/pull/9",
		State:   "OPEN",
		HeadSHA: "abcdef0123456789abcdef0123456789abcdef01",
		HeadRef: "feat/retry",
		BaseRef: "main",
	}
}

func TestBuildPRReviewPromptContract(t *testing.T) {
	o := prReviewOpts()
	o.Body = "why this change"
	o.Author = "dev"
	o.Additions, o.Deletions, o.ChangedFiles = 40, 7, 3
	p := BuildPRReviewPrompt(o)
	for _, want := range []string{
		"Alice",
		"acme/app",
		"#9",
		"add retry",
		"why this change",
		"feat/retry → main",
		"abcdef0123456789abcdef0123456789abcdef01",
		"+40 −7 across 3 files",
		"https://github.com/acme/app/pull/9",
		// The deliverable: one PR comment, posted by the agent itself.
		"gh pr comment 9 --repo acme/app --body-file",
		"gh pr diff 9 --repo acme/app",
		"git fetch origin pull/9/head",
		"multi-agent",
		"verifier",
		"fan out",
		"even when the PR looks clean",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
	// The whole point of this dispatch (vs commit review): comment, never issues,
	// and never a code change.
	for _, banned := range []string{"gh issue create"} {
		if strings.Contains(p, banned) {
			t.Fatalf("PR review prompt must not tell the agent to %q", banned)
		}
	}
	for _, want := range []string{
		"Do **not** open GitHub issues",
		"Do not edit files, do not commit, do not push",
		"never merge",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing prohibition %q in\n%s", want, p)
		}
	}
}

// A PR review is web-only: it streams onto the page that dispatched it and ends in
// a PR comment, so it must never open a Discord thread even with the gateway up.
func TestStartPRReviewNeverOpensDiscordThread(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextMsg: "m1", nextTh: "th-pr-review-1"}
	b.threadAPI = fake

	res, err := b.StartPRReview(prReviewOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if len(fake.starts) != 0 || len(fake.sends) != 0 {
		t.Fatalf("thread API must be untouched: starts=%v sends=%v", fake.starts, fake.sends)
	}
	if res.DiscordURL != "" {
		t.Fatalf("web-native must not set Discord URL: %+v", res)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Origin != SourceWeb || e.CreatedBy != "u1" {
		t.Fatalf("%+v", e)
	}
	// The goal is the only label a web-native unit gets — no thread name exists.
	if !strings.Contains(e.Goal, "acme/app#9") {
		t.Fatalf("goal=%q", e.Goal)
	}
	if e.DiscordURL != "" {
		t.Fatalf("discordURL=%q", e.DiscordURL)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

// Unlike Address CI / Address review, a review never reuses: it re-reads the head
// from scratch, so a session that already reasoned about an older diff is the wrong
// place to run it.
func TestStartPRReviewAlwaysCreatesNew(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := b.sessions.Set("bound-to-pr-9", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartPRReview(prReviewOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID == "bound-to-pr-9" {
		t.Fatalf("%+v", res)
	}
}

// The review unit binds the PR for the detail Sessions list, but is stamped
// SessionKindPRReview so FindByPR (Address reuse) never offers it.
func TestStartPRReviewBindsPRExcludedFromReuse(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	res, err := b.StartPRReview(prReviewOpts())
	if err != nil {
		t.Fatal(err)
	}
	e, _ := b.sessions.Get(res.ThreadID)
	e.NormalizePRs()
	if !e.IsPRReview() {
		t.Fatalf("want SessionKindPRReview, got kind=%q", e.SessionKind)
	}
	if len(e.PRs) != 1 || e.PRs[0].Number != 9 {
		t.Fatalf("review unit must bind the PR, got %+v", e.PRs)
	}
	if hits := b.FindByPR("app", "acme", "app", 9, true); len(hits) != 0 {
		t.Fatalf("review unit must not appear in the Address reuse picker, got %+v", hits)
	}
	// Detail Sessions list includes the review unit.
	listed := b.FindPRSessions("app", "acme", "app", 9)
	if len(listed) != 1 || listed[0].ThreadID != res.ThreadID {
		t.Fatalf("FindPRSessions: %+v", listed)
	}
	if listed[0].SessionKind != sessionstore.SessionKindPRReview {
		t.Fatalf("hit SessionKind=%q", listed[0].SessionKind)
	}
}

// A review resolves its model at creation, not at run start: the run path would
// otherwise re-resolve against the *task* model.
func TestStartPRReviewStampsReviewModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	res, err := b.StartPRReview(prReviewOpts())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5" {
		t.Fatalf("want review model stamped, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRReviewExplicitModelWins(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	o := prReviewOpts()
	o.Model = "grok-4.5"
	res, err := b.StartPRReview(o)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := b.sessions.Get(res.ThreadID)
	if e.Agent != "grok" || e.Model != "grok-4.5" {
		t.Fatalf("want explicit pick, got agent=%q model=%q", e.Agent, e.Model)
	}
}

func TestStartPRReviewRejectsUnknownModel(t *testing.T) {
	b, _ := testFixBot(t)
	o := prReviewOpts()
	o.Model = "gpt-9"
	if _, err := b.StartPRReview(o); err == nil || !strings.Contains(err.Error(), "gpt-9") {
		t.Fatalf("want unknown-model error naming the model, got %v", err)
	}
}

func TestStartPRReviewInvalidPR(t *testing.T) {
	b, _ := testFixBot(t)
	for _, tc := range []struct {
		name string
		mut  func(*PRReviewOpts)
	}{
		{"no number", func(o *PRReviewOpts) { o.Number = 0 }},
		{"no owner", func(o *PRReviewOpts) { o.Owner = "" }},
		{"no repo", func(o *PRReviewOpts) { o.Repo = "" }},
	} {
		o := prReviewOpts()
		tc.mut(&o)
		if _, err := b.StartPRReview(o); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}
