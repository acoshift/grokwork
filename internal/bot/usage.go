package bot

import (
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/history"
)

// turnUsage projects a finished run's token accounting onto its history record.
//
// The two halves come from different places on purpose, and swapping them is the
// one mistake here that produces plausible-looking wrong numbers.
//
// The billed classes come from Result.Usage, which for the claude driver is the
// CLI's `totalUsage` — a cumulative bill across every API call the run made. That
// is the right number for money: a cached prefix is re-sent and re-charged on
// every call, so the invoice counts it once per call, not once per run.
//
// Occupancy comes from Result.ContextTokensUsed, which the driver already took
// from the *last* API call alone (see claudeUsage.contextTokens). Deriving it from
// Usage instead — the obvious-looking Usage.PromptTokens() — would multiply it by
// roughly the number of assistant turns in the run: a real 2-turn run measured
// 16,826 cumulative against 8,445 actually resident.
//
// Returns nil when the run reported nothing, so an older record and a run that
// happened to cost nothing stay distinguishable.
func turnUsage(res grokrun.Result) *history.Usage {
	u := &history.Usage{
		ContextTokens:       res.ContextTokensUsed,
		ContextWindowTokens: res.ContextWindowTokens,
	}
	if res.Usage != nil {
		u.InputTokens = res.Usage.InputTokens
		u.CacheReadTokens = res.Usage.CacheReadInputTokens
		u.CacheCreationTokens = res.Usage.CacheCreationInputTokens
		u.OutputTokens = res.Usage.OutputTokens
		u.TotalTokens = res.Usage.TotalTokens
	}
	if u.IsZero() {
		return nil
	}
	return u
}
