package bot

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

// TestConcurrentClaimsOnDistinctThreadsDoNotDeadlock pins an AB-BA lock cycle.
//
// claimOrEnqueueInternal holds its own thread's threadState.mu across the
// concurrency-cap check, and that check scans every OTHER thread's
// threadState.mu to count active runs. Excluding the caller's own thread avoids
// self-deadlock on a non-reentrant mutex, but it does nothing about two
// goroutines claiming two different threads at once: goroutine A holds A.mu and
// blocks on B.mu while goroutine B holds B.mu and blocks on A.mu.
//
// It only bites when a cap is actually configured — an unset cap short-circuits
// before the scan — which is precisely the configuration the feature exists to
// enable.
//
// Many thread states are pre-created so each scan acquires many mutexes,
// widening the window in which two claimers overlap. With a per-thread window of
// only a few instructions the collision is rare; with ~64 locks per scan it is
// routine.
func TestConcurrentClaimsOnDistinctThreadsDoNotDeadlock(t *testing.T) {
	const threads = 64
	maxU := 1 << 20 // effectively unlimited: we are testing locking, not refusal
	b := &Bot{cfg: &config.Config{MaxConcurrentRunsUser: &maxU}}

	ids := make([]string, threads)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%02d", i)
		b.stateFor(ids[i]) // pre-create so scans have real mutexes to contend on
	}

	const rounds = 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range rounds {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i, id := range ids {
				wg.Go(func() {
					<-start // release all claimers at once
					b.claimOrEnqueue(id, &runJob{cancel: func() {}, start: time.Now()},
						taskItem{threadID: id, actor: Actor{ID: fmt.Sprintf("actor%d", i)}})
				})
			}
			close(start)
			wg.Wait()
			for _, id := range ids {
				b.finishRun(id) // back to st.job == nil, the branch that scans
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: concurrent claims on distinct threads did not complete — " +
			"the cap scan locks other threads' mu while holding its own")
	}
}
