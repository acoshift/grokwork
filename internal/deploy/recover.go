package deploy

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// StatusBlocked marks a run whose process tree outlived the restart. It is
// terminal for scheduling purposes: the lane must not be reused until a human
// looks, because something is still touching the environment.
const StatusBlocked Status = "blocked"

// RecoverAtStartup reconciles records left behind by a crash or restart.
//
// A deploy is NEVER auto-resumed. The agent runtime re-drives an interrupted
// turn with a "continue without duplicating completed steps" note, which works
// because a model turn is idempotent enough to retry; `kubectl apply` and
// `docker push` are not. Recovery here is a human clicking Redeploy with the
// failed step index in front of them.
//
// Must run before the server accepts triggers, so a recovered lane cannot race
// a fresh one.
func (e *Engine) RecoverAtStartup() (interrupted, cancelled, blocked int) {
	runs, err := e.store.List()
	if err != nil {
		log.Printf("warn: deploy recovery list: %v", err)
		return
	}
	for _, r := range runs {
		if r.Status.Terminal() || r.Status == StatusBlocked {
			continue
		}
		// Never re-drive another machine's work.
		if r.Host != "" && e.host != "" && r.Host != e.host {
			continue
		}
		// Probe the GROUP, not the leader: a crashed deploy whose `sh` exited
		// but whose children escaped is still touching the environment, and
		// would otherwise read as dead and let a new deploy race the orphan.
		if r.PID > 0 && processGroupAlive(r.PID) {
			// Deliberately no kill: this pid was recorded before a restart and
			// may since have been recycled by an unrelated process. Block the
			// lane and let a human decide.
			_ = e.store.Update(r.ID, func(rec *Run) error {
				rec.Status = StatusBlocked
				rec.Error = "process group from a previous run is still alive; not killed (the pid may have been recycled) — check the host before redeploying"
				rec.EndedAt = nowStamp()
				return nil
			})
			blocked++
			continue
		}
		_ = e.store.Update(r.ID, func(rec *Run) error {
			if rec.Status.Terminal() {
				return ErrSkipUpdate
			}
			if rec.Status == StatusPending {
				rec.Status = StatusCancelled
				rec.Error = "cancelled: the process restarted before this run started"
				cancelled++
			} else {
				rec.Status = StatusInterrupted
				rec.Error = "interrupted by a process restart; redeploy to run it again"
				interrupted++
			}
			rec.EndedAt = nowStamp()
			rec.PID = 0
			return nil
		})
	}
	e.sweepOrphanCheckouts()
	e.bump()
	if interrupted+cancelled+blocked > 0 {
		log.Printf("deploy: recovered interrupted=%d cancelled=%d blocked=%d", interrupted, cancelled, blocked)
	}
	return
}

// sweepOrphanCheckouts removes checkout directories with no live run.
//
// These live outside worktreesRoot precisely so the idle-worktree sweeper cannot
// see them, which means nothing else would ever clean them up.
func (e *Engine) sweepOrphanCheckouts() {
	root := filepath.Join(e.root, "checkouts")
	projects, err := os.ReadDir(root)
	if err != nil {
		return
	}
	live := map[string]bool{}
	if runs, err := e.store.List(); err == nil {
		for _, r := range runs {
			if !r.Status.Terminal() && r.Status != StatusBlocked {
				live[r.ID] = true
			}
		}
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() || live[ent.Name()] || !strings.HasPrefix(ent.Name(), "d_") {
				continue
			}
			path := filepath.Join(dir, ent.Name())
			if err := os.RemoveAll(path); err != nil {
				log.Printf("warn: deploy orphan checkout %s: %v", path, err)
			}
		}
	}
}
