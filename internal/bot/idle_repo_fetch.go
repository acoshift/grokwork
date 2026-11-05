package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// runRepoFetch is FetchNow. Tests replace it to observe the background call
// without a remote.
var runRepoFetch = gitworktree.FetchNow

// How often the idle-fetch loop wakes to re-check per-project intervals.
const idleRepoFetchTick = 1 * time.Minute

// Cap a single project's git fetch so a hung remote cannot block the loop forever.
const idleRepoFetchTimeout = 3 * time.Minute

// postMergeFetchAttempts covers a lock clash with terminal worktree cleanup.
// That cleanup runs git on the same checkout, in a goroutine started just
// before this fetch. Git fails the lock immediately. Idle fetch may be off,
// so a single failure would leave origin stale until someone fetches by hand.
const postMergeFetchAttempts = 3

const postMergeFetchRetry = time.Second

func (b *Bot) startIdleRepoFetch() {
	if b == nil {
		return
	}
	b.idleRepoFetchOnce.Do(func() {
		log.Printf("bg: starting idle-repo fetch tick=%s initial_delay=45s", idleRepoFetchTick)
		go b.runIdleRepoFetch()
	})
}

func (b *Bot) runIdleRepoFetch() {
	ctx := b.bgContext()
	log.Printf("bg: idle-repo fetch running (waiting 45s before first cycle)")
	if !sleepCtx(ctx, 45*time.Second) {
		log.Printf("bg: idle-repo fetch stopped before first cycle")
		return
	}
	b.runIdleRepoFetchCycle("initial")

	ticker := time.NewTicker(idleRepoFetchTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("bg: idle-repo fetch stopped")
			return
		case <-ticker.C:
			b.runIdleRepoFetchCycle("tick")
		}
	}
}

func (b *Bot) runIdleRepoFetchCycle(reason string) {
	if b == nil || b.cfg == nil {
		return
	}
	bg := b.bgContext()
	if bg.Err() != nil {
		return
	}
	targets := b.cfg.IdleRepoFetchTargets()
	if len(targets) == 0 {
		return
	}
	start := time.Now()
	var fetched, throttled, skipped, failed int
	for _, t := range targets {
		if bg.Err() != nil {
			log.Printf("bg: idle-repo fetch cycle aborted reason=%s (stopping)", reason)
			break
		}
		if t.Interval <= 0 || t.Path == "" {
			skipped++
			continue
		}
		if !gitworktree.IsRepo(t.Path) {
			skipped++
			continue
		}
		ctx, cancel := context.WithTimeout(bg, idleRepoFetchTimeout)
		ran, err := gitworktree.MaybeFetch(ctx, t.Path, t.Interval)
		cancel()
		if err != nil {
			if bg.Err() != nil {
				break
			}
			failed++
			log.Printf("warn: idle-repo fetch project=%s path=%s: %v", t.Name, t.Path, err)
			continue
		}
		if ran {
			fetched++
		} else {
			throttled++
		}
	}
	log.Printf("bg: idle-repo fetch cycle reason=%s projects=%d fetched=%d throttled=%d skipped=%d failed=%d elapsed=%s",
		reason, len(targets), fetched, throttled, skipped, failed, time.Since(start).Round(time.Millisecond))
}

// ScheduleRepoFetch refreshes cwd's origin refs without blocking the caller.
// A merge this process just performed moved origin. Idle fetch may be
// disabled or minutes away, and the commits page, deploy board, and new
// worktrees read origin/*. The fetch uses the bot lifetime so shutdown
// cancels it. Errors are logged; they do not affect the merge that already
// landed. cwd is the catalog checkout for that repo, not a session worktree.
func (b *Bot) ScheduleRepoFetch(cwd, label string) {
	if b == nil {
		return
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return
	}
	label = strings.TrimSpace(label)
	go b.repoFetch(cwd, label)
}

func (b *Bot) repoFetch(cwd, label string) {
	bg := b.bgContext()
	if bg.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(bg, idleRepoFetchTimeout)
	defer cancel()
	start := time.Now()
	var ran bool
	var err error
	for attempt := range postMergeFetchAttempts {
		if attempt > 0 && !sleepCtx(bg, postMergeFetchRetry) {
			return
		}
		ran, err = runRepoFetch(ctx, cwd)
		if err == nil || bg.Err() != nil {
			break
		}
	}
	if err != nil {
		if bg.Err() != nil {
			return
		}
		log.Printf("warn: repo fetch %s path=%s: %v", label, cwd, err)
		return
	}
	if !ran {
		log.Printf("bg: repo fetch %s skipped (not a git checkout)", label)
		return
	}
	log.Printf("bg: repo fetch %s elapsed=%s", label, time.Since(start).Round(time.Millisecond))
}
