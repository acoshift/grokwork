package bot

import (
	"strings"
	"testing"
	"time"

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

func TestStartCommitReviewCreateCallsThreadAPI(t *testing.T) {
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
	if !res.Created || res.ThreadID != "th-review-1" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 1 || len(fake.sends) != 1 {
		t.Fatalf("starts=%v sends=%v", fake.starts, fake.sends)
	}
	if !strings.Contains(fake.sends[0], "abcdef0") || !strings.Contains(fake.sends[0], "Alice") {
		t.Fatalf("starter=%v", fake.sends)
	}
	e, ok := b.sessions.Get("th-review-1")
	if !ok || e.Origin != SourceWeb || e.CreatedBy != "u1" {
		t.Fatalf("%+v", e)
	}
	if e.Goal == "" || !strings.Contains(e.Goal, "abcdef0") {
		t.Fatalf("goal=%q", e.Goal)
	}
	if e.DiscordURL == "" || !strings.Contains(e.DiscordURL, "guild-1") {
		t.Fatalf("discordURL=%q", e.DiscordURL)
	}
	waitHistory(t, b, "th-review-1", 1)
}

func TestStartCommitReviewAlwaysCreatesNew(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// Pre-existing session must not be reused.
	if err := b.sessions.Set("old-review", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeThreadAPI{nextTh: "th-review-2"}
	b.threadAPI = fake

	res, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ShortSHA: "deadbee",
		Subject: "again", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID != "th-review-2" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 1 {
		t.Fatal("expected one create")
	}
}

func TestStartCommitReviewDiscordDownWebNative(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	res, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Owner: "acme", Repo: "app",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ShortSHA: "aaaaaaa",
		Subject: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatalf("web-native create should succeed: %v", err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if res.DiscordURL != "" {
		t.Fatalf("web-native must not set Discord URL: %+v", res)
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
