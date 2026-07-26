package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

// TestPerUserConcurrentRunCapEnforced pins the actual bug: countActiveRunsByUser
// used to be a hardcoded "return 0" stub, so MaxConcurrentRunsUser could never
// fire. With real counting, a second concurrent run by the same actor must be
// refused while a different actor (well under the cap) is still let through.
func TestPerUserConcurrentRunCapEnforced(t *testing.T) {
	max := 1
	b := &Bot{cfg: &config.Config{MaxConcurrentRunsUser: &max}}

	job1 := &runJob{cancel: func() {}, start: time.Now()}
	claimed, _, err := b.claimOrEnqueue("t1", job1, taskItem{threadID: "t1", actor: Actor{ID: "alice"}})
	if err != nil || !claimed {
		t.Fatalf("alice first claim: claimed=%v err=%v", claimed, err)
	}

	// Same actor, a second (idle) thread: must be refused by the per-user cap.
	job2 := &runJob{cancel: func() {}, start: time.Now()}
	claimed, _, err = b.claimOrEnqueue("t2", job2, taskItem{threadID: "t2", actor: Actor{ID: "alice"}})
	if claimed || err == nil {
		t.Fatalf("expected per-user cap to refuse alice's second run, claimed=%v err=%v", claimed, err)
	}
	if !strings.Contains(err.Error(), "per-user concurrent run limit") {
		t.Fatalf("unexpected error: %v", err)
	}

	// A different actor is nowhere near the cap and must still be allowed.
	job3 := &runJob{cancel: func() {}, start: time.Now()}
	claimed, _, err = b.claimOrEnqueue("t3", job3, taskItem{threadID: "t3", actor: Actor{ID: "bob"}})
	if err != nil || !claimed {
		t.Fatalf("expected bob to be allowed, claimed=%v err=%v", claimed, err)
	}
}

// TestPerUserConcurrentRunCapIgnoresQueued mirrors the host-wide counter's
// meaning: only ACTIVE runs count, so an actor whose only presence is a
// queued (not yet running) follow-up must not be blocked from claiming a
// fresh thread even when the per-user cap is 1.
func TestPerUserConcurrentRunCapIgnoresQueued(t *testing.T) {
	b := &Bot{}

	// "holder" occupies thread t1.
	holderJob := &runJob{cancel: func() {}, start: time.Now()}
	if claimed, _, err := b.claimOrEnqueue("t1", holderJob, taskItem{threadID: "t1", actor: Actor{ID: "holder"}}); err != nil || !claimed {
		t.Fatalf("holder claim: claimed=%v err=%v", claimed, err)
	}

	// "alice" tries the busy thread and lands in the queue, not active.
	claimed, pos, err := b.claimOrEnqueue("t1", &runJob{cancel: func() {}}, taskItem{threadID: "t1", actor: Actor{ID: "alice"}})
	if err != nil || claimed || pos != 1 {
		t.Fatalf("alice enqueue: claimed=%v pos=%d err=%v", claimed, pos, err)
	}
	if n := b.countActiveRunsByUser("alice"); n != 0 {
		t.Fatalf("queued item counted as active for alice: %d", n)
	}

	// Now turn on a per-user cap of 1 and confirm alice — whose only presence
	// anywhere is that queued item — can still claim a brand-new thread.
	max := 1
	b.cfg = &config.Config{MaxConcurrentRunsUser: &max}
	claimed, _, err = b.claimOrEnqueue("t2", &runJob{cancel: func() {}, start: time.Now()}, taskItem{threadID: "t2", actor: Actor{ID: "alice"}})
	if err != nil || !claimed {
		t.Fatalf("alice claim on fresh thread should not be blocked by her queued item: claimed=%v err=%v", claimed, err)
	}
}

// TestPerUserConcurrentRunCapAllowsSameThreadFollowUp pins that the per-user
// cap check runs only on the claim path (a NEW active run), not ahead of the
// busy-thread/queue branch. With MaxConcurrentRunsUser=1, alice's own
// already-running job on t1 is the one active run counted for her; a second
// @Grok message on that SAME thread is an ordinary mid-run follow-up and must
// be queued behind it, not refused as hitting her own cap.
func TestPerUserConcurrentRunCapAllowsSameThreadFollowUp(t *testing.T) {
	max := 1
	b := &Bot{cfg: &config.Config{MaxConcurrentRunsUser: &max}}

	job1 := &runJob{cancel: func() {}, start: time.Now()}
	claimed, _, err := b.claimOrEnqueue("t1", job1, taskItem{threadID: "t1", actor: Actor{ID: "alice"}})
	if err != nil || !claimed {
		t.Fatalf("alice first claim: claimed=%v err=%v", claimed, err)
	}

	// Alice sends a follow-up to the SAME thread t1: must queue, not be
	// refused by the cap she's already "using" via her own active job.
	job2 := &runJob{cancel: func() {}}
	claimed, pos, err := b.claimOrEnqueue("t1", job2, taskItem{threadID: "t1", actor: Actor{ID: "alice"}})
	if err != nil || claimed || pos != 1 {
		t.Fatalf("alice same-thread follow-up should queue, got claimed=%v pos=%d err=%v", claimed, pos, err)
	}
}

// TestHostConcurrentRunCapAllowsSameThreadFollowUp mirrors
// TestPerUserConcurrentRunCapAllowsSameThreadFollowUp for the host-wide cap:
// the single active run on a thread must not block a follow-up destined for
// that same thread's queue.
func TestHostConcurrentRunCapAllowsSameThreadFollowUp(t *testing.T) {
	max := 1
	b := &Bot{cfg: &config.Config{MaxConcurrentRuns: &max}}

	job1 := &runJob{cancel: func() {}, start: time.Now()}
	claimed, _, err := b.claimOrEnqueue("t1", job1, taskItem{threadID: "t1", actor: Actor{ID: "alice"}})
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}

	job2 := &runJob{cancel: func() {}}
	claimed, pos, err := b.claimOrEnqueue("t1", job2, taskItem{threadID: "t1", actor: Actor{ID: "bob"}})
	if err != nil || claimed || pos != 1 {
		t.Fatalf("same-thread follow-up should queue, got claimed=%v pos=%d err=%v", claimed, pos, err)
	}
}

// TestCountActiveRunsByUserNormalizesActorID pins that a bare Discord id and
// its "discord:" namespaced spelling land in the same bucket, so a config
// written either way and a runtime id written the other way still collide.
func TestCountActiveRunsByUserNormalizesActorID(t *testing.T) {
	b := &Bot{}
	job := &runJob{cancel: func() {}, start: time.Now()}
	if claimed, _, err := b.claimOrEnqueue("t1", job, taskItem{threadID: "t1", actor: Actor{ID: "123"}}); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if n := b.countActiveRunsByUser("discord:123"); n != 1 {
		t.Fatalf("countActiveRunsByUser(\"discord:123\")=%d want 1", n)
	}
	if n := b.countActiveRunsByUser("123"); n != 1 {
		t.Fatalf("countActiveRunsByUser(\"123\")=%d want 1", n)
	}
	if n := b.countActiveRunsByUser("456"); n != 0 {
		t.Fatalf("countActiveRunsByUser(\"456\")=%d want 0", n)
	}
}
