package bot

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/timeline"
)

// Events returns the per-unit timeline store (may be nil if init failed).
func (b *Bot) Events() *timeline.Store {
	if b == nil {
		return nil
	}
	return b.events
}

// appendTimeline records one event, best-effort.
//
// Persistence is deliberately decoupled from rendering: a surface failure must
// never fail a run, and a store failure must never fail a surface. Errors are
// logged against the unit and the run continues either way.
func (b *Bot) appendTimeline(unitID string, kind timeline.Kind, data any) {
	if b == nil || b.events == nil || strings.TrimSpace(unitID) == "" {
		return
	}
	if _, err := b.events.Append(unitID, kind, data); err != nil {
		if errors.Is(err, timeline.ErrFull) {
			// Bounded coverage is logged, never silent: a capped timeline that
			// looks complete is worse than one that says it stopped.
			log.Printf("timeline: unit=%s cap reached — %s event dropped", unitID, kind)
			return
		}
		log.Printf("warn: timeline append unit=%s kind=%s: %v", unitID, kind, err)
	}
}

// recordRunTimeline persists a finished run's assistant output and outcome.
//
// This is what makes output survive a run that produced no final reply. The
// Discord path never needed it — every sealed chunk was already sitting in the
// thread — but history.Turn.Response is assigned result.Text with no fallback,
// so a cancelled or max-turns run left a web-native unit with nothing to show.
//
// Called unconditionally, outside every `present`/`!result.Cancelled` branch:
// the cancelled case is the one that most needs the record.
func (b *Bot) recordRunTimeline(unitID string, streamedText string, result grokrun.Result, elapsed time.Duration) {
	if b == nil || b.events == nil {
		return
	}

	// One block per run, not per Discord message. Chunking upstream exists only
	// because Discord caps a message at 1900 chars; the timeline has no such cap,
	// so re-splitting here would encode a foreign constraint into the store.
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = strings.TrimSpace(streamedText)
	}
	if text != "" {
		b.appendTimeline(unitID, timeline.KindTextBlock, timeline.TextBlock{Text: text})
	}

	status, errMsg := runTimelineOutcome(result)
	b.appendTimeline(unitID, timeline.KindRunDone, timeline.RunDone{
		Status:   status,
		Error:    errMsg,
		Elapsed:  formatElapsed(elapsed),
		ExitCode: result.Code,
	})
}

// runTimelineOutcome mirrors the status/error derivation in
// recordTurnActorPolicy so the timeline and history cannot disagree about how a
// run ended.
func runTimelineOutcome(result grokrun.Result) (status, errMsg string) {
	switch {
	case result.Cancelled:
		return "cancelled", "Cancelled"
	case result.MaxTurnsReached:
		return "error", "Reached max turns before a final reply"
	case result.Code != 0:
		return "error", historyErrorFromResult(result)
	}
	return "done", ""
}
