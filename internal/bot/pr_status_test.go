package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPreservePRFields(t *testing.T) {
	prev := sessionstore.Entry{
		PRURL:          "https://github.com/o/r/pull/3",
		PRNumber:       3,
		PRState:        "OPEN",
		PRTitle:        "t",
		PRChecks:       "✓ 1",
		PRReview:       "APPROVED",
		PRHeadSHA:      "abc",
		PRIsDraft:      true,
		PRStatusMsgID:  "msg-1",
		CINotifiedSHA:  "abc",
		CIAutoFixCount: 1,
		CIAutoFixSHA:   "abc",
		Goal:           "fix it",
		BriefMsgID:     "brief-1",
		Origin:         SourceWeb,
		CreatedBy:      "web-user-1",
		CreatedByName:  "Web Alice",
		DiscordURL:     "https://discord.com/channels/1/2",
	}
	next := sessionstore.Entry{
		SessionID: "s",
		Project:   "p",
	}
	preservePRFields(&next, prev)
	if next.PRNumber != 3 || next.PRStatusMsgID != "msg-1" || !next.PRIsDraft {
		t.Fatalf("next=%+v", next)
	}
	if next.CINotifiedSHA != "abc" || next.CIAutoFixCount != 1 || next.CIAutoFixSHA != "abc" {
		t.Fatalf("ci fields not preserved: %+v", next)
	}
	if next.Goal != "fix it" || next.BriefMsgID != "brief-1" {
		t.Fatalf("brief fields not preserved: %+v", next)
	}
	if next.Origin != SourceWeb || next.CreatedBy != "web-user-1" || next.CreatedByName != "Web Alice" || next.DiscordURL == "" {
		t.Fatalf("workflow fields not preserved via preservePRFields: %+v", next)
	}
	if next.SessionID != "s" {
		t.Fatalf("clobbered session: %+v", next)
	}
}

func TestEntryPRInfoStatusLines(t *testing.T) {
	e := sessionstore.Entry{
		PRNumber:  9,
		PRURL:     "https://github.com/o/r/pull/9",
		PRState:   "OPEN",
		PRIsDraft: false,
		PRChecks:  "✓ 2",
		PRReview:  "REVIEW_REQUIRED",
	}
	e.NormalizePRs()
	lines := ghpr.FormatMultiStatusLines(entryPRInfos(e))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"#9", "OPEN", "✓ 2", "REVIEW_REQUIRED"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestMultiPRUpsertAndStatus(t *testing.T) {
	e := sessionstore.Entry{SessionID: "s", Project: "p"}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/a/pull/1", Number: 1, State: "OPEN", Owner: "acoshift", Repo: "a",
	})
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/b/pull/2", Number: 2, State: "OPEN", Owner: "acoshift", Repo: "b",
	})
	if len(e.PRs) != 2 {
		t.Fatalf("prs=%d", len(e.PRs))
	}
	if !e.HasOpenPR() || e.AllPRsTerminal() {
		t.Fatal("expected open PRs")
	}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/a/pull/1", Number: 1, State: "MERGED", Owner: "acoshift", Repo: "a",
	})
	if len(e.OpenPRs()) != 1 {
		t.Fatalf("open=%d", len(e.OpenPRs()))
	}
	lines := ghpr.FormatMultiStatusLines(entryPRInfos(e))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "2 tracked") || !strings.Contains(joined, "acoshift/b#2") {
		t.Fatalf("status=%q", joined)
	}
}

func TestPrRepoDirPrefersWorktree(t *testing.T) {
	// Empty paths → empty (no real git dirs in unit test).
	if got := prRepoDir(sessionstore.Entry{}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestPrViewCwdFallsBackWithoutGitRoot(t *testing.T) {
	// Multi-repo project root: directory exists but is not a git worktree.
	// Poller used to skip these forever (session PR stuck at OPEN after merge).
	root := t.TempDir()
	proj := filepath.Join(root, "monorepo-parent")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nested real-ish layout without initializing git at parent.
	if err := os.MkdirAll(filepath.Join(proj, "apiserver"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"deploys": proj}),
		DataDir:  root,
	}
	b := New(cfg, nil, nil)

	e := sessionstore.Entry{
		Project: "deploys",
		Cwd:     proj,
		MainCwd: proj,
	}
	if got := prRepoDir(e); got != "" {
		t.Fatalf("prRepoDir should be empty for non-git parent, got %q", got)
	}
	got := b.prViewCwd(e)
	if got != proj {
		t.Fatalf("prViewCwd=%q want project path %q", got, proj)
	}

	// Session paths missing → still use configured project path.
	e2 := sessionstore.Entry{Project: "deploys"}
	if got := b.prViewCwd(e2); got != proj {
		t.Fatalf("prViewCwd from config=%q want %q", got, proj)
	}
}

func TestPrViewSelectorPrefersURL(t *testing.T) {
	sel := prViewSelector(sessionstore.TrackedPR{
		Number: 244,
		Owner:  "deploys-app",
		Repo:   "apiserver",
		// No URL — should still build one for non-git cwd polling.
	})
	if sel != "https://github.com/deploys-app/apiserver/pull/244" {
		t.Fatalf("sel=%q", sel)
	}
	sel = prViewSelector(sessionstore.TrackedPR{
		URL:    "https://github.com/deploys-app/apiserver/pull/244",
		Number: 244,
	})
	if sel != "https://github.com/deploys-app/apiserver/pull/244" {
		t.Fatalf("url sel=%q", sel)
	}
}

// Regression: cleanup must not run while the job is still held (executeTask path),
// and must run once the thread is idle (finishRun → tryCleanupTerminalPR).
func TestTryCleanupTerminalPRDefersWhenBusy(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	threadID := "thread-1"
	if err := store.Set(threadID, sessionstore.Entry{
		SessionID: "s1",
		Project:   "p",
		PRNumber:  7,
		PRState:   "MERGED",
		PRURL:     "https://github.com/o/r/pull/7",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate active run (same as during executeTask / refreshPRAfterTask).
	job := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	if _, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil {
		t.Fatal(err)
	}
	b.tryCleanupTerminalPR(threadID)
	if _, ok := store.Get(threadID); !ok {
		t.Fatal("session deleted while busy — eager cleanup raced the active job")
	}

	// Release job (finishRun with empty queue).
	if _, ok := b.finishRun(threadID); ok {
		t.Fatal("expected no queued next")
	}
	b.tryCleanupTerminalPR(threadID)
	if _, ok := store.Get(threadID); ok {
		t.Fatal("session should be deleted after idle terminal cleanup")
	}
}
