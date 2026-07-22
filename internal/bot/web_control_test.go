package bot

import (
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestCancelRunIdle(t *testing.T) {
	b, _ := testFixBot(t)
	msg, ok := b.CancelRun("idle-thread", "Alice")
	if ok {
		t.Fatalf("idle thread must report ok=false, msg=%q", msg)
	}
	if msg == "" {
		t.Fatal("expected an idle message")
	}
}

func TestResetUnitBusyRefusal(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "busy-reset"
	if err := SeedActiveRunForTest(b, threadID, "app", "prompt", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { FinishRunForTest(b, threadID) })

	if _, err := b.ResetUnit(threadID); err == nil {
		t.Fatal("expected reset to refuse while busy")
	}
}

func TestRemoveQueuedTaskByID(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "dequeue-th"
	// Hold an active job, then enqueue one follow-up with a known TaskID/author.
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID, authorID: "holder"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	t.Cleanup(func() { FinishRunForTest(b, threadID) })
	if _, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, taskID: "task-A", authorID: "alice", authorName: "Alice",
		parsed: Parsed{Prompt: "do a thing"},
	}); err != nil {
		t.Fatal(err)
	}

	// Wrong TaskID → no-op error, queue unchanged.
	if err := b.RemoveQueuedTask(threadID, "task-Z", "alice", false); err == nil {
		t.Fatal("wrong task id should error")
	}
	if n := b.queueLen(threadID); n != 1 {
		t.Fatalf("queue mutated on wrong id: len=%d", n)
	}

	// Correct TaskID but not author and no control → permission denied, unchanged.
	if err := b.RemoveQueuedTask(threadID, "task-A", "mallory", false); err == nil {
		t.Fatal("non-author without control must be denied")
	}
	if n := b.queueLen(threadID); n != 1 {
		t.Fatalf("queue mutated on denial: len=%d", n)
	}

	// Author removes own item.
	if err := b.RemoveQueuedTask(threadID, "task-A", "alice", false); err != nil {
		t.Fatalf("author remove: %v", err)
	}
	if n := b.queueLen(threadID); n != 0 {
		t.Fatalf("queue not drained: len=%d", n)
	}
}

func TestRemoveQueuedTaskControlOverride(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "dequeue-ctrl"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID, authorID: "holder"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	t.Cleanup(func() { FinishRunForTest(b, threadID) })
	if _, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, taskID: "task-B", authorID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	// canControl=true lets a non-author (owner/admin) remove it.
	if err := b.RemoveQueuedTask(threadID, "task-B", "someone-else", true); err != nil {
		t.Fatalf("control override: %v", err)
	}
	if n := b.queueLen(threadID); n != 0 {
		t.Fatalf("queue not drained: len=%d", n)
	}
}

func TestQueueItemsProjection(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "qitems"
	if b.QueueItems(threadID) != nil {
		t.Fatal("idle thread must report no queue items")
	}
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID, authorID: "holder"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	t.Cleanup(func() { FinishRunForTest(b, threadID) })
	if _, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, taskID: "t1", authorID: "alice", authorName: "Alice",
		parsed: Parsed{Prompt: "fix the parser"},
	}); err != nil {
		t.Fatal(err)
	}
	items := b.QueueItems(threadID)
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	got := items[0]
	if got.TaskID != "t1" || got.AuthorID != "alice" || got.AuthorName != "Alice" || got.Position != 1 {
		t.Fatalf("%+v", got)
	}
	if got.Mode != "fix" { // empty snapMode defaults to fix
		t.Fatalf("mode=%q", got.Mode)
	}
	if got.Intent == "" {
		t.Fatalf("intent empty: %+v", got)
	}
}

func TestSetSessionLabel(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "label-th"
	if err := b.sessions.Set(threadID, sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSessionLabel(threadID, "blocked"); err != nil {
		t.Fatal(err)
	}
	e, _ := b.sessions.Get(threadID)
	if e.Label != sessionstore.LabelBlocked || !e.LabelManual {
		t.Fatalf("%+v", e)
	}
	// "auto" clears the manual override.
	if err := b.SetSessionLabel(threadID, "auto"); err != nil {
		t.Fatal(err)
	}
	e, _ = b.sessions.Get(threadID)
	if e.LabelManual {
		t.Fatalf("manual not cleared: %+v", e)
	}
	// Unknown label → error, no change.
	if err := b.SetSessionLabel(threadID, "bogus"); err == nil {
		t.Fatal("expected unknown label error")
	}
	// Missing session → error.
	if err := b.SetSessionLabel("no-such-thread", "open"); err == nil {
		t.Fatal("expected missing-session error")
	}
}

// Manual /label is the documented escape hatch for closed cases (case_cmd.go's
// closed message says "ask eng to /label"), so a manual set must succeed even on
// a closed case — mirroring Discord's handleLabel, which never refuses it. The
// K18 auto-label freeze (sessionstore.ApplyAutoLabel) is what still protects the
// board from PR-driven relabels; it is unaffected.
func TestSetSessionLabelClosedCaseManualAllowed(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "closed-case"
	if err := b.sessions.Set(threadID, sessionstore.Entry{
		Project: "app", Mode: "case", Phase: sessionstore.PhaseClosed, Label: sessionstore.LabelDone,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSessionLabel(threadID, "open"); err != nil {
		t.Fatalf("manual label on closed case must be allowed: %v", err)
	}
	e, _ := b.sessions.Get(threadID)
	if e.Label != sessionstore.LabelOpen || !e.LabelManual {
		t.Fatalf("manual label not applied on closed case: %+v", e)
	}
}

func TestSetSessionGoal(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "goal-th"
	if err := b.sessions.Set(threadID, sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSessionGoal(threadID, "  ship the login rework  "); err != nil {
		t.Fatal(err)
	}
	e, _ := b.sessions.Get(threadID)
	if e.Goal != "ship the login rework" {
		t.Fatalf("goal=%q", e.Goal)
	}
	if err := b.SetSessionGoal(threadID, "   "); err == nil {
		t.Fatal("empty goal should error")
	}
	if err := b.SetSessionGoal("no-such-thread", "x"); err == nil {
		t.Fatal("missing session should error")
	}
}

func TestClaimThreadDemotesOwner(t *testing.T) {
	b, _ := testFixBot(t)
	const threadID = "claim-th"
	e := sessionstore.Entry{Project: "app"}
	e.SetOwner("old", "Old Owner")
	if err := b.sessions.Set(threadID, e); err != nil {
		t.Fatal(err)
	}
	if err := b.ClaimThread(threadID, Actor{ID: "new", DisplayName: "New Owner"}); err != nil {
		t.Fatal(err)
	}
	got, _ := b.sessions.Get(threadID)
	if got.OwnerID != "new" {
		t.Fatalf("owner=%q want new", got.OwnerID)
	}
	if !got.IsCoOwner("old") {
		t.Fatalf("previous owner not demoted to co-owner: %+v", got)
	}
	if len(got.CoOwnerIDs) != 1 {
		t.Fatalf("co-owner list should be exactly the previous owner: %+v", got.CoOwnerIDs)
	}
	// Re-claim by the same owner is a no-op (no error, no growth).
	if err := b.ClaimThread(threadID, Actor{ID: "new", DisplayName: "New Owner"}); err != nil {
		t.Fatal(err)
	}
	got, _ = b.sessions.Get(threadID)
	if len(got.CoOwnerIDs) != 1 {
		t.Fatalf("co-owner list grew on self-claim: %+v", got.CoOwnerIDs)
	}
}

func TestClaimThreadCreatesShell(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.ClaimThread("no-session-yet", Actor{ID: "owner1", DisplayName: "One"}); err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get("no-session-yet")
	if !ok || e.OwnerID != "owner1" {
		t.Fatalf("shell not created with owner: ok=%v %+v", ok, e)
	}
}

func TestClaimThreadAuthOffNoIdentity(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.ClaimThread("th", Actor{}); err == nil {
		t.Fatal("claim without identity must error")
	}
}
