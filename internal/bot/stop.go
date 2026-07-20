package bot

import (
	"context"
	"log"
	"os"

	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/runjournal"
)

// Stop cancels in-flight runs, checkpoints journals as interrupted, and waits for drains.
// If Active.Status is cancelling, it is left as cancelling (not forced to interrupted).
func (b *Bot) Stop(ctx context.Context) {
	if b == nil {
		return
	}
	b.stopping.Store(true)
	log.Printf("stop: cancelling active runs…")

	b.states.Range(func(key, value any) bool {
		st, _ := value.(*threadState)
		if st == nil {
			return true
		}
		st.mu.Lock()
		job := st.job
		st.mu.Unlock()
		if job != nil && job.cancel != nil {
			job.cancel()
		}
		return true
	})

	done := make(chan struct{})
	go func() {
		b.drainWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("stop: all drains finished")
	case <-ctx.Done():
		log.Printf("stop: timeout; journals marked interrupted where possible; orphans left for Recover")
	}

	if b.runs != nil {
		list, err := b.runs.List()
		if err != nil {
			log.Printf("stop: list journals: %v", err)
		} else {
			for _, j := range list {
				tid := j.ThreadID
				_ = b.runs.Update(tid, func(jj *runjournal.Journal) error {
					if jj.GrokPID != 0 {
						if grokrun.ProcessAlive(jj.GrokPID) && runjournal.LooksLikeGrokCLI(jj.GrokPID, b.cfg.GrokBin) {
							grokrun.KillProcessGroup(jj.GrokPID)
						}
						jj.GrokPID = 0
					}
					if jj.Active != nil {
						// Preserve cancelling so recover drops user-cancelled work.
						if jj.Active.Status != runjournal.StatusCancelling {
							jj.Active.Status = runjournal.StatusInterrupted
						}
					}
					jj.BlockedReason = ""
					return nil
				})
			}
		}
		_ = b.runs.Unlock(os.Getpid())
	}
	log.Printf("stop: done")
}
