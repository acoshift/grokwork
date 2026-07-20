package bot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func testFixBot(t *testing.T) (*Bot, string) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		GrokBin:        writeFakeGrok(t),
		Projects:       config.PathProjects(map[string]string{"app": proj}),
		Channels:       map[string]string{"ch-app": "app"},
		DiscordGuildID: "guild-1",
		DataDir:        filepath.Join(dir, "data"),
		ConfigPath:     filepath.Join(dir, "config.json"),
		WorktreeIsolation: boolPtr(false),
		MaxTurns:       5,
		TimeoutMs:      5000,
		Yolo:           boolPtr(true),
	}
	// preferred channel = mapped
	pc := cfg.Projects["app"]
	pc.DiscordChannelID = "ch-app"
	// enable linear for linear tests
	pc.Linear = &config.ProjectLinearConfig{Enabled: true, APIKey: "lin-key", TeamKey: "ENG"}
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

func TestBuildGitHubFixPromptContract(t *testing.T) {
	p := BuildGitHubFixPrompt("Alice", "acme", "app", 7, "Bug", "https://github.com/acme/app/issues/7", "body text")
	for _, want := range []string{
		"Alice", "acme/app#7", "Bug", "body text", "Fixes acme/app#7", "Do not merge",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestBuildLinearFixPromptContract(t *testing.T) {
	p := BuildLinearFixPrompt("Bob", "ENG-9", "Title", "https://linear.app/x/issue/ENG-9", "Todo", "desc")
	for _, want := range []string{
		"Bob", "ENG-9", "Title", "desc", "Fixes ENG-9", "Do not merge", "Do not call Linear issueUpdate",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestStartFixPickerNoCreate(t *testing.T) {
	b, _ := testFixBot(t)
	fake := &fakeThreadAPI{nextTh: "should-not-create"}
	b.threadAPI = fake

	for _, id := range []string{"a", "b"} {
		e := sessionstore.Entry{Project: "app"}
		e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 1, Keyword: sessionstore.IssueKeywordFixes})
		if err := b.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 1,
		Title: "T", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if !errors.Is(err, ErrPickerRequired) {
		t.Fatalf("err=%v", err)
	}
	if res.Status != FixStatusPicker || len(res.Hits) != 2 {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatalf("create must not run: %v", fake.starts)
	}
}

func TestStartFixReuseNoCreate(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "new-th"}
	b.threadAPI = fake

	e := sessionstore.Entry{Project: "app", Origin: SourceWeb}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 5})
	if err := b.sessions.Set("exist-1", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 5,
		Title: "Fix me", Body: "details",
		Actor: Actor{ID: "u9", DisplayName: "WebU"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.ThreadID != "exist-1" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 0 {
		t.Fatal("reuse must not createWorkflowThread")
	}
	// Fixes keyword bound
	got, ok := b.sessions.Get("exist-1")
	if !ok || len(got.Issues) != 1 || got.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordFixes {
		t.Fatalf("%+v", got)
	}
	// Wait for async start
	waitHistory(t, b, "exist-1", 1)
}

func TestStartFixCreateCallsThreadAPIOnce(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextMsg: "m1", nextTh: "th-new-42"}
	b.threadAPI = fake

	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 42,
		Title: "Payment bug", Body: "repro",
		Actor: Actor{ID: "u1", DisplayName: "Alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID != "th-new-42" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 1 || len(fake.sends) != 1 {
		t.Fatalf("starts=%v sends=%v", fake.starts, fake.sends)
	}
	if !strings.Contains(fake.sends[0], "42") && !strings.Contains(fake.sends[0], "Alice") {
		t.Fatalf("starter=%v", fake.sends)
	}
	e, ok := b.sessions.Get("th-new-42")
	if !ok || e.Origin != SourceWeb || e.CreatedBy != "u1" {
		t.Fatalf("%+v", e)
	}
	if e.DiscordURL == "" || !strings.Contains(e.DiscordURL, "guild-1") {
		t.Fatalf("discordURL=%q", e.DiscordURL)
	}
	if len(e.Issues) != 1 || e.Issues[0].Number != 42 {
		t.Fatalf("issues=%+v", e.Issues)
	}
	waitHistory(t, b, "th-new-42", 1)
}

func TestStartFixForceNewBypassesReuse(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "forced"}
	b.threadAPI = fake
	e := sessionstore.Entry{Project: "app"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 3})
	if err := b.sessions.Set("old", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 3,
		Title: "again", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID != "forced" {
		t.Fatalf("%+v", res)
	}
	if len(fake.starts) != 1 {
		t.Fatal("expected create")
	}
}

func TestStartFixCreateDiscordDown(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// no threadAPI, no Discord session → web-native unit
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 1,
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
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
	e, ok := b.sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("session missing")
	}
	if !strings.HasPrefix(e.WorktreeBranch, gitworktree.WebBranchPrefix) {
		t.Fatalf("branch=%q want web prefix", e.WorktreeBranch)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

func TestStartFixReuseDiscordDownStillEnqueues(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// no Discord, no threadAPI
	e := sessionstore.Entry{Project: "app"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 8})
	if err := b.sessions.Set("reuse-off", e); err != nil {
		t.Fatal(err)
	}
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 8,
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DiscordOffline || res.Created {
		t.Fatalf("%+v", res)
	}
	waitHistory(t, b, "reuse-off", 1)
}

func TestStartFixQueueFull(t *testing.T) {
	b, _ := testFixBot(t)
	// Hold active + fill queue
	threadID := "qfull"
	e := sessionstore.Entry{Project: "app"}
	e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 11})
	if err := b.sessions.Set(threadID, e); err != nil {
		t.Fatal(err)
	}
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil || !claimed {
		t.Fatal(err)
	}
	for i := 0; i < maxFollowupQueue; i++ {
		if claimed, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{threadID: threadID}); err != nil || claimed {
			t.Fatalf("fill %d: %v %v", i, claimed, err)
		}
	}
	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 11,
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err=%v", err)
	}
	b.clearQueue(threadID)
	b.finishRun(threadID)
}

func TestStartFixLinearDisabled(t *testing.T) {
	b, _ := testFixBot(t)
	pc := b.cfg.Projects["app"]
	pc.Linear = &config.ProjectLinearConfig{Enabled: false}
	b.cfg.Projects["app"] = pc
	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindLinear, Project: "app", Identifier: "ENG-1",
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if !errors.Is(err, ErrLinearDisabled) {
		t.Fatalf("%v", err)
	}
}

func TestStartFixLinearCreate(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	fake := &fakeThreadAPI{nextTh: "lin-th"}
	b.threadAPI = fake
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindLinear, Project: "app", Identifier: "ENG-55",
		Title: "Lin bug", Body: "steps", State: "Todo",
		URL: "https://linear.app/x/issue/ENG-55",
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ThreadID != "lin-th" {
		t.Fatalf("%+v", res)
	}
	e, _ := b.sessions.Get("lin-th")
	if len(e.Issues) != 1 || !e.Issues[0].IsLinear() || e.Issues[0].Identifier != "ENG-55" {
		t.Fatalf("%+v", e.Issues)
	}
	if e.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordFixes {
		t.Fatal("want Fixes")
	}
}

func TestStartFixChannelMissing(t *testing.T) {
	b, _ := testFixBot(t)
	b.threadAPI = &fakeThreadAPI{nextTh: "x"}
	// Remove channel mapping
	b.cfg.Channels = map[string]string{}
	pc := b.cfg.Projects["app"]
	pc.DiscordChannelID = ""
	b.cfg.Projects["app"] = pc
	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "a", Repo: "b", Number: 1,
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err == nil {
		t.Fatal("expected channel error")
	}
}

func TestStartFixPickerThenThreadID(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	for _, id := range []string{"p1", "p2"} {
		e := sessionstore.Entry{Project: "app"}
		e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 2})
		if err := b.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	// pick p2
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ThreadID: "p2",
		Owner: "acme", Repo: "app", Number: 2,
		Title: "t", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID != "p2" || res.Created {
		t.Fatalf("%+v", res)
	}
	waitHistory(t, b, "p2", 1)
}

func waitHistory(t *testing.T, b *Bot, threadID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		th, err := b.history.Get(threadID)
		if err == nil && len(th.Turns) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	th, _ := b.history.Get(threadID)
	t.Fatalf("timeout history thread=%s turns=%+v", threadID, th)
}
