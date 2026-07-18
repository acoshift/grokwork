package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/ghpr"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func TestPreservePRFields(t *testing.T) {
	prev := sessionstore.Entry{
		PRURL:         "https://github.com/o/r/pull/3",
		PRNumber:      3,
		PRState:       "OPEN",
		PRTitle:       "t",
		PRChecks:      "✓ 1",
		PRReview:      "APPROVED",
		PRHeadSHA:     "abc",
		PRIsDraft:     true,
		PRStatusMsgID: "msg-1",
	}
	next := sessionstore.Entry{
		SessionID: "s",
		Project:   "p",
	}
	preservePRFields(&next, prev)
	if next.PRNumber != 3 || next.PRStatusMsgID != "msg-1" || !next.PRIsDraft {
		t.Fatalf("next=%+v", next)
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
	lines := ghpr.FormatStatusLines(entryPRInfo(e))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"#9", "OPEN", "✓ 2", "REVIEW_REQUIRED"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestPrRepoDirPrefersWorktree(t *testing.T) {
	// Empty paths → empty (no real git dirs in unit test).
	if got := prRepoDir(sessionstore.Entry{}); got != "" {
		t.Fatalf("got %q", got)
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
