package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestBuildCommitReviewPromptContract(t *testing.T) {
	p := BuildCommitReviewPrompt(CommitReviewOpts{
		Actor:    Actor{DisplayName: "Alice"},
		Owner:    "acme",
		Repo:     "app",
		SHA:      "abcdef0123456789abcdef0123456789abcdef01",
		ShortSHA: "abcdef0",
		Subject:  "fix nil deref",
		Body:     "details here",
		Author:   "Dev <d@ex.com>",
		Date:     "2026-07-20",
	})
	for _, want := range []string{
		"Alice",
		"acme/app",
		"abcdef0123456789abcdef0123456789abcdef01",
		"abcdef0",
		"fix nil deref",
		"details here",
		"gh issue create",
		"commit-review",
		"severity:",
		"https://github.com/acme/app/commit/abcdef0123456789abcdef0123456789abcdef01",
		"bot will not file issues",
		"multi-agent",
		"verifier",
		"fan out",
		"git show --stat",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

// A commit review is web-only: the session it creates streams onto the page that
// dispatched it and ends in GitHub issues, so it must never open a Discord thread
// even when the gateway is up and a channel is mapped.
func TestStartCommitReviewNeverOpensDiscordThread(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextMsg: "m1", nextTh: "th-review-1"}
	b.threadAPI = fake

	res, err := b.StartCommitReview(CommitReviewOpts{
		Project:  "app",
		Owner:    "acme",
		Repo:     "app",
		SHA:      "abcdef0123456789abcdef0123456789abcdef01",
		ShortSHA: "abcdef0",
		Subject:  "ship feature",
		Actor:    Actor{ID: "u1", DisplayName: "Alice"},
	})
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
	if e.Goal == "" || !strings.Contains(e.Goal, "abcdef0") {
		t.Fatalf("goal=%q", e.Goal)
	}
	if e.DiscordURL != "" {
		t.Fatalf("discordURL=%q", e.DiscordURL)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartCommitReviewAlwaysCreatesNew(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// Pre-existing session must not be reused.
	if err := b.sessions.Set("old-review", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ShortSHA: "deadbee",
		Subject: "again", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID == "old-review" {
		t.Fatalf("%+v", res)
	}
}

// A review resolves its model at creation, not at run start: the run path would
// otherwise re-resolve against the *task* model and the review model would only
// ever apply by coincidence.
func TestStartCommitReviewStampsReviewModel(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5-high", ReviewModel: "claude-opus-5-high",
	})
	res, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ShortSHA: "aaaaaaa",
		Subject: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || e.Agent != "claude" || e.Model != "claude-opus-5-high" {
		t.Fatalf("want review model stamped, got agent=%q model=%q", e.Agent, e.Model)
	}
}

// An explicit pick from the dispatch modal beats the configured review model.
func TestStartCommitReviewExplicitModelWins(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5-high", ReviewModel: "claude-opus-5-high",
	})
	res, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ShortSHA: "bbbbbbb",
		Subject: "t", Actor: Actor{ID: "u", DisplayName: "U"},
		Model: "grok-4.5-high",
	})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := b.sessions.Get(res.ThreadID)
	if e.Agent != "grok" || e.Model != "grok-4.5-high" {
		t.Fatalf("want explicit pick, got agent=%q model=%q", e.Agent, e.Model)
	}
}

// A name that is not on the curated list never reaches the CLI.
func TestStartCommitReviewRejectsUnknownModel(t *testing.T) {
	b, _ := testFixBot(t)
	_, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", ShortSHA: "ccccccc",
		Actor: Actor{ID: "u", DisplayName: "U"}, Model: "gpt-9",
	})
	if err == nil || !strings.Contains(err.Error(), "gpt-9") {
		t.Fatalf("want unknown-model error naming the model, got %v", err)
	}
}

func TestStartCommitReviewMissingSHA(t *testing.T) {
	b, _ := testFixBot(t)
	_, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
