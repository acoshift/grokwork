package ghpr

import (
	"fmt"
	"strings"
)

// TimelineKind is a discrete PR lifecycle transition worth posting in Discord.
type TimelineKind string

const (
	TimelineApproved          TimelineKind = "approved"
	TimelineChangesRequested  TimelineKind = "changes_requested"
	TimelineCIGreen           TimelineKind = "ci_green"
	TimelineMerged            TimelineKind = "merged"
	TimelineClosed            TimelineKind = "closed"
)

// TimelineEvent is one transition detected between two PR snapshots.
type TimelineEvent struct {
	Kind   TimelineKind
	Detail string // optional extra (e.g. checks rollup)
}

// Snapshot is the comparable PR surface for timeline diffs (poller state machine).
type Snapshot struct {
	State  string
	Review string
	Checks string
}

// SnapshotFromInfo builds a Snapshot from a live gh view.
func SnapshotFromInfo(info Info) Snapshot {
	return Snapshot{
		State:  info.State,
		Review: info.ReviewDecision,
		Checks: info.Checks,
	}
}

// DiffTimeline returns notable transitions from prev → next.
//
// First observation (prev completely empty): only terminal state is announced
// (PR already merged/closed while untracked). Review/CI events require a
// prior snapshot so the poller does not spam on first card seed.
func DiffTimeline(prev, next Snapshot) []TimelineEvent {
	prevState := strings.ToUpper(strings.TrimSpace(prev.State))
	nextState := strings.ToUpper(strings.TrimSpace(next.State))
	prevReview := strings.ToUpper(strings.TrimSpace(prev.Review))
	nextReview := strings.ToUpper(strings.TrimSpace(next.Review))
	prevChecks := strings.TrimSpace(prev.Checks)
	nextChecks := strings.TrimSpace(next.Checks)

	first := prevState == "" && prevReview == "" && prevChecks == ""

	var out []TimelineEvent

	// Terminal state (merged / closed). Announce even on first observation.
	if IsTerminal(nextState) && !IsTerminal(prevState) {
		switch nextState {
		case "MERGED":
			out = append(out, TimelineEvent{Kind: TimelineMerged})
		case "CLOSED":
			out = append(out, TimelineEvent{Kind: TimelineClosed})
		}
	}

	if first {
		return out
	}

	// Review decision transitions (not first seed).
	if nextReview != "" && nextReview != prevReview {
		switch nextReview {
		case "APPROVED":
			out = append(out, TimelineEvent{Kind: TimelineApproved})
		case "CHANGES_REQUESTED":
			out = append(out, TimelineEvent{Kind: TimelineChangesRequested})
		}
	}

	// CI green: had fail or pending, now all passing (no ✗ / …).
	if !IsTerminal(nextState) && ChecksAllGreen(nextChecks) && !ChecksAllGreen(prevChecks) && checksHadSignal(prevChecks) {
		detail := nextChecks
		if detail == "" || strings.EqualFold(detail, "none") {
			detail = ""
		}
		out = append(out, TimelineEvent{Kind: TimelineCIGreen, Detail: detail})
	}

	return out
}

// ChecksAllGreen reports a checks rollup with at least one pass and no fail/pending.
// "none" / empty is not green (no CI signal).
func ChecksAllGreen(summary string) bool {
	s := strings.TrimSpace(summary)
	if s == "" || strings.EqualFold(s, "none") {
		return false
	}
	if strings.Contains(s, "✗") || strings.Contains(s, "…") {
		return false
	}
	return strings.Contains(s, "✓")
}

func checksHadSignal(summary string) bool {
	s := strings.TrimSpace(summary)
	if s == "" || strings.EqualFold(s, "none") {
		return false
	}
	// Any prior rollup (pass, fail, pending, or mix) counts as a signal.
	return true
}

// FormatTimeline builds a short Discord message for one or more PR events.
func FormatTimeline(info Info, events []TimelineEvent) string {
	if len(events) == 0 {
		return ""
	}
	label := fmt.Sprintf("#%d", info.Number)
	if info.Owner != "" && info.Repo != "" {
		label = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
	}
	lines := []string{fmt.Sprintf("**PR event** · %s", label)}
	for _, ev := range events {
		lines = append(lines, "• "+formatTimelineLine(ev))
	}
	if u := strings.TrimSpace(info.URL); u != "" {
		// Only append URL on terminal events (card already has the link).
		for _, ev := range events {
			if ev.Kind == TimelineMerged || ev.Kind == TimelineClosed {
				lines = append(lines, u)
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func formatTimelineLine(ev TimelineEvent) string {
	switch ev.Kind {
	case TimelineApproved:
		return "Review: **APPROVED**"
	case TimelineChangesRequested:
		return "Review: **CHANGES_REQUESTED**"
	case TimelineCIGreen:
		if d := strings.TrimSpace(ev.Detail); d != "" {
			return "CI: **green** · " + d
		}
		return "CI: **green**"
	case TimelineMerged:
		return "State: **MERGED**"
	case TimelineClosed:
		return "State: **CLOSED**"
	default:
		if d := strings.TrimSpace(ev.Detail); d != "" {
			return string(ev.Kind) + ": " + d
		}
		return string(ev.Kind)
	}
}

// HasTerminalTimeline reports whether events include merged or closed.
func HasTerminalTimeline(events []TimelineEvent) bool {
	for _, ev := range events {
		if ev.Kind == TimelineMerged || ev.Kind == TimelineClosed {
			return true
		}
	}
	return false
}
