package bot

import (
	"context"
	"time"
)

// bgContext returns the bot-lifetime context cancelled by Stop. Safe when
// New was never called with a full bot (returns Background).
func (b *Bot) bgContext() context.Context {
	if b == nil || b.bgCtx == nil {
		return context.Background()
	}
	return b.bgCtx
}

// sleepCtx waits d or until ctx is done. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
