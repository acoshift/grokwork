package bot

import (
	"context"
	"log"
	"time"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// How often the idle-fetch loop wakes to re-check per-project intervals.
const idleRepoFetchTick = 1 * time.Minute

// Cap a single project's git fetch so a hung remote cannot block the loop forever.
const idleRepoFetchTimeout = 3 * time.Minute

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
