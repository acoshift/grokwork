package bot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func testAddressBot(t *testing.T) (*Bot, string) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		GrokBin: writeFakeGrok(t),
		// Never leave ClaudeBin unset: it normalizes to "claude" and execs the real
		// CLI — see writeFakeClaude.
		ClaudeBin:         writeFakeClaude(t),
		Projects:          config.PathProjects(map[string]string{"app": proj}),
		Channels:          map[string]string{"ch-app": "app"},
		DiscordGuildID:    "guild-1",
		DataDir:           filepath.Join(dir, "data"),
		ConfigPath:        filepath.Join(dir, "config.json"),
		WorktreeIsolation: boolPtr(false),
		MaxTurns:          5,
		TimeoutMs:         5000,
		Yolo:              boolPtr(true),
	}
	pc := cfg.Projects["app"]
	pc.DiscordChannelID = "ch-app"
	cfg.Projects["app"] = pc
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, hist), proj
}

func TestFindByPR(t *testing.T) {
	b, store := testBotSessions(t)
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := store.Set("pr-th", e); err != nil {
		t.Fatal(err)
	}
	done := sessionstore.Entry{Project: "app", Label: sessionstore.LabelDone}
	done.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9})
	if err := store.Set("pr-done", done); err != nil {
		t.Fatal(err)
	}
	hits := b.FindByPR("app", "acme", "app", 9, false)
	if len(hits) != 1 || hits[0].ThreadID != "pr-th" {
		t.Fatalf("%+v", hits)
	}
	if len(b.FindByPR("app", "acme", "app", 9, true)) != 2 {
		t.Fatal("include terminal")
	}
}

func TestBuildAddressCIPromptSinglePRNoMerge(t *testing.T) {
	p := BuildAddressCIPrompt(AddressCIOpts{
		Actor: Actor{DisplayName: "Alice"},
		Owner: "acme", Repo: "app", Number: 4,
		Title: "T", URL: "https://github.com/acme/app/pull/4",
		HeadRef: "grok/discord/x",
		Failed:  []ghpr.Check{{Name: "ci", Link: "https://x"}},
	})
	for _, want := range []string{
		"Alice", "acme/app#4", "this pull request only", "Do not merge", "ci",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
	// Must not claim multi-PR loop language from Discord multi path
	if strings.Contains(p, "pull requests linked to this Discord") {
		t.Fatal("must be single-PR scoped")
	}
}

func TestBuildAddressReviewPromptNoMerge(t *testing.T) {
	p := BuildAddressReviewPrompt(AddressReviewOpts{
		Actor: Actor{DisplayName: "Bob"},
		Owner: "acme", Repo: "app", Number: 2,
		Comments: []ghpr.ReviewComment{
			{Path: "f.go", Line: 3, Body: "nil check", Author: "r", URL: "u"},
		},
	})
	for _, want := range []string{"Bob", "acme/app#2", "nil check", "f.go", "Do not merge"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

// A dispatch from the PR page creates a web-native session: it streams onto the
// session page the POST redirects to, and the PR is already where the team watches
// this work — so no Discord thread is opened even with the gateway up.
func TestStartAddressCICreateUpsertsPR(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "ci-th"}
	b.threadAPI = fake
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 12,
		Title: "ci", Actor: Actor{ID: "u", DisplayName: "U"},
		Failed: []ghpr.Check{{Name: "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatalf("thread API must be untouched: %v", fake.starts)
	}
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok || len(e.PRs) != 1 || e.PRs[0].Number != 12 {
		t.Fatalf("PR not bound: %+v", e)
	}
	// bindTrackedPR carries no goal, and there is no thread name to fall back on.
	if !strings.Contains(e.Goal, "acme/app#12") {
		t.Fatalf("goal=%q", e.Goal)
	}
}

// A reused session keeps the agent it was stamped with: a session id cannot be
// resumed by the other CLI, so a model named on the dispatch must not reach it.
func TestStartAddressCIReuseKeepsPinnedModel(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	e := sessionstore.Entry{Project: "app", Agent: "grok", Model: "grok-4.5", SessionID: "s-1"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 5, State: "OPEN"})
	if err := b.sessions.Set("exist-pinned", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 5,
		Actor: Actor{ID: "u", DisplayName: "U"}, Model: "claude-opus-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID != "exist-pinned" {
		t.Fatalf("want reuse, got %+v", res)
	}
	got, _ := b.sessions.Get("exist-pinned")
	if got.Agent != "grok" || got.Model != "grok-4.5" {
		t.Fatalf("reuse re-pinned the session: agent=%q model=%q", got.Agent, got.Model)
	}
}

func TestStartAddressCICreateStampsReviewModel(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	setAgentSettingsKeepBins(t, b.cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 21,
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := b.sessions.Get(res.ThreadID)
	if got.Agent != "claude" || got.Model != "claude-opus-5" {
		t.Fatalf("want review model stamped, got agent=%q model=%q", got.Agent, got.Model)
	}
}

func TestStartAddressCIReuseNoCreate(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "nope"}
	b.threadAPI = fake
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 5, State: "OPEN"})
	if err := b.sessions.Set("exist-ci", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 5,
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.ThreadID != "exist-ci" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatal("reuse must not create")
	}
}

func TestStartAddressCIPicker(t *testing.T) {
	b, _ := testAddressBot(t)
	fake := &fakeThreadAPI{nextTh: "x"}
	b.threadAPI = fake
	for _, id := range []string{"a", "b"} {
		e := sessionstore.Entry{Project: "app"}
		e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 1})
		if err := b.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 1,
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if !errors.Is(err, ErrPickerRequired) || res.Status != FixStatusPicker {
		t.Fatalf("%v %+v", err, res)
	}
	if len(fake.starts) != 0 {
		t.Fatal("no create")
	}
}

func TestStartAddressCIDiscordDown(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	res, err := b.StartAddressCI(AddressCIOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 1,
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatalf("web-native address create: %v", err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("%+v", res)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartContinueNoCreate(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "x"}
	b.threadAPI = fake
	if err := b.sessions.Set("cont-1", sessionstore.Entry{Project: "app", Origin: SourceWeb}); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartContinue(ContinueOpts{
		ThreadID: "cont-1", Prompt: "keep going on the tests",
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.ThreadID != "cont-1" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatal("continue must not create")
	}
	waitHistory(t, b, "cont-1", 1)
	th, _ := b.history.Get("cont-1")
	if !strings.Contains(th.Turns[0].Prompt, "keep going") {
		t.Fatalf("%q", th.Turns[0].Prompt)
	}
	if !strings.Contains(strings.ToLower(th.Turns[0].Prompt), "do not merge") {
		t.Fatalf("want do not merge: %q", th.Turns[0].Prompt)
	}
}

func TestStartContinueUnknownThread(t *testing.T) {
	b, _ := testAddressBot(t)
	_, err := b.StartContinue(ContinueOpts{ThreadID: "missing", Prompt: "x", Actor: Actor{ID: "u"}})
	if !errors.Is(err, ErrUnknownThread) {
		t.Fatalf("%v", err)
	}
}

func TestStartAddressReviewCreate(t *testing.T) {
	b, _ := testAddressBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "rev-th"}
	b.threadAPI = fake
	res, err := b.StartAddressReview(AddressReviewOpts{
		Project: "app", Owner: "acme", Repo: "app", Number: 8,
		Actor:    Actor{ID: "u", DisplayName: "U"},
		Comments: []ghpr.ReviewComment{{Path: "x.go", Body: "fix me", Author: "r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native created unit, got %+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatalf("thread API must be untouched: %v", fake.starts)
	}
	e, _ := b.sessions.Get(res.ThreadID)
	if len(e.PRs) != 1 || e.PRs[0].Number != 8 {
		t.Fatalf("%+v", e.PRs)
	}
}

func TestStartAddressReviewEmptyComments(t *testing.T) {
	b, _ := testAddressBot(t)
	_, err := b.StartAddressReview(AddressReviewOpts{
		Project: "app", Owner: "a", Repo: "b", Number: 1,
		Actor: Actor{ID: "u"}, Comments: nil,
	})
	if !errors.Is(err, ErrNoReviewComments) {
		t.Fatalf("%v", err)
	}
}
