package bot

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// How often the idle-fetch loop wakes to re-check per-project intervals.
const idleRepoFetchTick = 1 * time.Minute

// Cap a single project's git fetch so a hung remote cannot block the loop forever.
const idleRepoFetchTimeout = 3 * time.Minute

var idleRepoFetchOnce sync.Once

func (b *Bot) startIdleRepoFetch() {
	idleRepoFetchOnce.Do(func() {
		log.Printf("bg: starting idle-repo fetch tick=%s initial_delay=45s", idleRepoFetchTick)
		go b.runIdleRepoFetch()
	})
}

func (b *Bot) runIdleRepoFetch() {
	log.Printf("bg: idle-repo fetch running (waiting 45s before first cycle)")
	time.Sleep(45 * time.Second)
	b.runIdleRepoFetchCycle("initial")

	ticker := time.NewTicker(idleRepoFetchTick)
	defer ticker.Stop()
	for range ticker.C {
		b.runIdleRepoFetchCycle("tick")
	}
}

func (b *Bot) runIdleRepoFetchCycle(reason string) {
	if b == nil || b.cfg == nil {
		return
	}
	targets := b.cfg.IdleRepoFetchTargets()
	if len(targets) == 0 {
		return
	}
	start := time.Now()
	var fetched, throttled, skipped, failed int
	for _, t := range targets {
		if t.Interval <= 0 || t.Path == "" {
			skipped++
			continue
		}
		if !gitworktree.IsRepo(t.Path) {
			skipped++
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), idleRepoFetchTimeout)
		ran, err := gitworktree.MaybeFetch(ctx, t.Path, t.Interval)
		cancel()
		if err != nil {
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
