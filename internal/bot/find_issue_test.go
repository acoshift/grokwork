package bot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func testBotSessions(t *testing.T) (*Bot, *sessionstore.Store) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		GrokBin:    "false",
		Projects:   config.PathProjects(map[string]string{"app": proj}),
		Channels:   map[string]string{"ch1": "app"},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, hist), store
}

func TestFindByIssueMatchAndTerminalFilter(t *testing.T) {
	b, store := testBotSessions(t)
	// Matching open
	e1 := sessionstore.Entry{Project: "app", Goal: "fix pay"}
	e1.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42, Keyword: sessionstore.IssueKeywordFixes})
	if err := store.Set("t-open", e1); err != nil {
		t.Fatal(err)
	}
	// Terminal done — excluded by default
	e2 := sessionstore.Entry{Project: "app", Label: sessionstore.LabelDone}
	e2.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42})
	if err := store.Set("t-done", e2); err != nil {
		t.Fatal(err)
	}
	// Wrong issue
	e3 := sessionstore.Entry{Project: "app"}
	e3.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 99})
	if err := store.Set("t-other", e3); err != nil {
		t.Fatal(err)
	}
	// Wrong project
	e4 := sessionstore.Entry{Project: "other"}
	e4.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 42})
	if err := store.Set("t-proj", e4); err != nil {
		t.Fatal(err)
	}
	// Unbound free-text only — not matched
	if err := store.Set("t-unbound", sessionstore.Entry{Project: "app", Goal: "see #42"}); err != nil {
		t.Fatal(err)
	}

	hits := b.FindByIssue("app", "acme", "app", 42, false)
	if len(hits) != 1 || hits[0].ThreadID != "t-open" {
		t.Fatalf("hits=%+v", hits)
	}
	// include terminal
	hitsAll := b.FindByIssue("app", "acme", "app", 42, true)
	if len(hitsAll) != 2 {
		t.Fatalf("includeTerminal hits=%d", len(hitsAll))
	}
}

func TestActiveFixGitHubIssues(t *testing.T) {
	b, store := testBotSessions(t)
	// Active Fixes bind
	e1 := sessionstore.Entry{Project: "app", Label: sessionstore.LabelInProgress}
	e1.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 7, Keyword: sessionstore.IssueKeywordFixes})
	if err := store.Set("t-fix", e1); err != nil {
		t.Fatal(err)
	}
	// Refs only — not fixing
	e2 := sessionstore.Entry{Project: "app", Label: sessionstore.LabelInProgress}
	e2.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 8, Keyword: sessionstore.IssueKeywordRefs})
	if err := store.Set("t-refs", e2); err != nil {
		t.Fatal(err)
	}
	// Terminal — not fixing
	e3 := sessionstore.Entry{Project: "app", Label: sessionstore.LabelDone}
	e3.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 9, Keyword: sessionstore.IssueKeywordFixes})
	if err := store.Set("t-done", e3); err != nil {
		t.Fatal(err)
	}

	got := b.ActiveFixGitHubIssues("app", "acme", "app")
	if _, ok := got[7]; !ok {
		t.Fatalf("want #7 fixing, got %v", got)
	}
	if _, ok := got[8]; ok {
		t.Fatalf("Refs should not count as fixing: %v", got)
	}
	if _, ok := got[9]; ok {
		t.Fatalf("terminal session should not count: %v", got)
	}
}

func TestActiveFixLinearIssues(t *testing.T) {
	b, store := testBotSessions(t)
	e := sessionstore.Entry{Project: "app", Label: sessionstore.LabelOpen}
	e.UpsertIssue(sessionstore.TrackedIssue{
		Provider:   sessionstore.ProviderLinear,
		Identifier: "eng-42",
		Keyword:    sessionstore.IssueKeywordFixes,
	})
	if err := store.Set("t-lin", e); err != nil {
		t.Fatal(err)
	}
	got := b.ActiveFixLinearIssues("app")
	if _, ok := got["ENG-42"]; !ok {
		t.Fatalf("want ENG-42, got %v", got)
	}
}

func TestFindByIssueOrderingBusyWorktreeNewest(t *testing.T) {
	b, store := testBotSessions(t)
	now := time.Now().UTC()
	mk := func(id, updated string, branch string) {
		e := sessionstore.Entry{
			Project: "app", UpdatedAt: updated, WorktreeBranch: branch,
		}
		e.UpsertIssue(sessionstore.TrackedIssue{Owner: "o", Repo: "r", Number: 1})
		// Set bypasses UpdatedAt rewrite with fixed value — use Patch after Set... 
		// Set always rewrites UpdatedAt. Sleep or rely on order of Set for newest.
		if err := store.Set(id, e); err != nil {
			t.Fatal(err)
		}
		// Force UpdatedAt via direct patch of file is hard; use sequential Set and thread ids.
		_ = now
	}
	mk("old", "", "")
	time.Sleep(5 * time.Millisecond)
	mk("with-wt", "", "grok/discord/with-wt")
	time.Sleep(5 * time.Millisecond)
	mk("busy-idle", "", "")
	// Mark busy
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue("busy-idle", job, taskItem{threadID: "busy-idle"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	hits := b.FindByIssue("app", "o", "r", 1, false)
	if len(hits) != 3 {
		t.Fatalf("hits=%d", len(hits))
	}
	if hits[0].ThreadID != "busy-idle" || !hits[0].Busy {
		t.Fatalf("want busy first: %+v", hits[0])
	}
	// Among non-busy, worktree preferred over empty
	if hits[1].ThreadID != "with-wt" {
		t.Fatalf("want worktree second: %+v", hits)
	}
	b.finishRun("busy-idle")
}

func TestFindByLinearIssue(t *testing.T) {
	b, store := testBotSessions(t)
	e := sessionstore.Entry{Project: "app"}
	e.UpsertIssue(sessionstore.TrackedIssue{
		Provider: sessionstore.ProviderLinear, Identifier: "ENG-123", Keyword: sessionstore.IssueKeywordFixes,
	})
	if err := store.Set("lin-1", e); err != nil {
		t.Fatal(err)
	}
	hits := b.FindByLinearIssue("app", "eng-123", false)
	if len(hits) != 1 || hits[0].ThreadID != "lin-1" {
		t.Fatalf("%+v", hits)
	}
	if len(b.FindByLinearIssue("app", "ENG-999", false)) != 0 {
		t.Fatal("no match")
	}
}

func TestDiscordThreadURL(t *testing.T) {
	if DiscordThreadURL("", "t") != "" || DiscordThreadURL("g", "") != "" {
		t.Fatal("empty")
	}
	u := DiscordThreadURL("g1", "t9")
	if u != "https://discord.com/channels/g1/t9" {
		t.Fatalf("%q", u)
	}
}
