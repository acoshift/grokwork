package bot

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestClaimOrEnqueueAndDrain(t *testing.T) {
	b := &Bot{}
	const threadID = "t1"

	job1 := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	item1 := taskItem{threadID: threadID, proj: projectRef{Name: "p"}}
	claimed, pos, err := b.claimOrEnqueue(threadID, job1, item1)
	if err != nil || !claimed || pos != 0 {
		t.Fatalf("first claim: claimed=%v pos=%d err=%v", claimed, pos, err)
	}

	job2 := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	item2 := taskItem{threadID: threadID, proj: projectRef{Name: "p"}, parsed: Parsed{Prompt: "follow-up-1"}}
	claimed, pos, err = b.claimOrEnqueue(threadID, job2, item2)
	if err != nil || claimed || pos != 1 {
		t.Fatalf("second enqueue: claimed=%v pos=%d err=%v", claimed, pos, err)
	}
	if n := b.queueLen(threadID); n != 1 {
		t.Fatalf("queueLen=%d want 1", n)
	}

	if j, ok := b.getJob(threadID); !ok || j != job1 {
		t.Fatalf("getJob: ok=%v job=%v", ok, j)
	}

	next, ok := b.finishRun(threadID)
	if !ok || next.parsed.Prompt != "follow-up-1" {
		t.Fatalf("finishRun next=%+v ok=%v", next, ok)
	}
	if n := b.queueLen(threadID); n != 0 {
		t.Fatalf("queueLen after pop=%d", n)
	}
	if _, ok := b.getJob(threadID); !ok {
		t.Fatal("expected still busy while draining")
	}

	jobNext := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	b.replaceJob(threadID, jobNext)
	if j, ok := b.getJob(threadID); !ok || j != jobNext {
		t.Fatalf("replaceJob: ok=%v", ok)
	}

	if _, ok := b.finishRun(threadID); ok {
		t.Fatal("expected no more queued")
	}
	if _, ok := b.getJob(threadID); ok {
		t.Fatal("expected idle after final finishRun")
	}
}

func TestQueueFull(t *testing.T) {
	b := &Bot{}
	const threadID = "t-full"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	item := taskItem{threadID: threadID, authorID: "holder"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, item); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	for i := range maxFollowupQueue {
		dummy := &runJob{cancel: func() {}}
		claimed, pos, err := b.claimOrEnqueue(threadID, dummy, taskItem{
			threadID: threadID,
			authorID: "u" + string(rune('a'+i)),
			parsed:   Parsed{Prompt: "q"},
		})
		if err != nil || claimed || pos != i+1 {
			t.Fatalf("enqueue %d: claimed=%v pos=%d err=%v", i, claimed, pos, err)
		}
	}
	dummy := &runJob{cancel: func() {}}
	claimed, _, err := b.claimOrEnqueue(threadID, dummy, taskItem{threadID: threadID, authorID: "other"})
	if err != errQueueFull || claimed {
		t.Fatalf("want queue full, claimed=%v err=%v", claimed, err)
	}
}

func TestSameUserQueueAppends(t *testing.T) {
	b := &Bot{}
	const threadID = "t-append"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID, authorID: "hold"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	_, pos, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, authorID: "alice", authorName: "Alice",
		parsed: Parsed{Prompt: "first follow-up"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Fatalf("pos=%d want 1", pos)
	}
	_, pos, err = b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, authorID: "alice", authorName: "Alice",
		parsed: Parsed{Prompt: "second follow-up"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 2 {
		t.Fatalf("pos=%d want 2 (appended)", pos)
	}
	if n := b.queueLen(threadID); n != 2 {
		t.Fatalf("queueLen=%d want 2", n)
	}
	q := b.queueSnapshot(threadID)
	if q[0].parsed.Prompt != "first follow-up" || q[1].parsed.Prompt != "second follow-up" {
		t.Fatalf("queue prompts = %q, %q", q[0].parsed.Prompt, q[1].parsed.Prompt)
	}
}

func TestSameUserQueueCanFillThreadCap(t *testing.T) {
	b := &Bot{}
	const threadID = "t-alice-full"
	job := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	if claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID, authorID: "alice"}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	for i := range maxFollowupQueue {
		claimed, pos, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
			threadID: threadID, authorID: "alice",
			parsed:   Parsed{Prompt: "q"},
		})
		if err != nil || claimed || pos != i+1 {
			t.Fatalf("enqueue %d: claimed=%v pos=%d err=%v", i, claimed, pos, err)
		}
	}
	claimed, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{
		threadID: threadID, authorID: "alice", parsed: Parsed{Prompt: "overflow"},
	})
	if err != errQueueFull || claimed {
		t.Fatalf("want queue full, claimed=%v err=%v", claimed, err)
	}
}

func TestClearQueue(t *testing.T) {
	b := &Bot{}
	const threadID = "t-clear"
	job := &runJob{cancel: func() {}, start: time.Now()}
	if _, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := b.claimOrEnqueue(threadID, &runJob{cancel: func() {}}, taskItem{threadID: threadID}); err != nil {
			t.Fatal(err)
		}
	}
	if n := b.clearQueue(threadID); n != 3 {
		t.Fatalf("clearQueue=%d want 3", n)
	}
	if n := b.queueLen(threadID); n != 0 {
		t.Fatalf("queueLen=%d", n)
	}
	if _, ok := b.getJob(threadID); !ok {
		t.Fatal("expected job still active")
	}
}

func TestClaimOrEnqueueConcurrent(t *testing.T) {
	b := &Bot{}
	const threadID = "t-race"
	var wg sync.WaitGroup
	var claimedCount, queuedCount, fullCount int
	var mu sync.Mutex

	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(t.Context())
			_ = ctx
			job := &runJob{cancel: cancel, start: time.Now()}
			claimed, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == errQueueFull:
				fullCount++
				cancel()
			case claimed:
				claimedCount++
			default:
				queuedCount++
				cancel()
			}
		})
	}
	wg.Wait()
	if claimedCount != 1 {
		t.Fatalf("claimedCount=%d want 1", claimedCount)
	}
	if queuedCount+fullCount != 19 {
		t.Fatalf("queued=%d full=%d want sum 19", queuedCount, fullCount)
	}
	if queuedCount > maxFollowupQueue {
		t.Fatalf("queued=%d > max %d", queuedCount, maxFollowupQueue)
	}
}
