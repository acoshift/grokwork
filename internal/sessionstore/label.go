package sessionstore

import (
	"fmt"
	"strings"
)

// Thread lifecycle labels (team workflow). Empty Label is treated as open.
const (
	LabelOpen        = "open"
	LabelInProgress  = "in_progress"
	LabelBlocked     = "blocked"
	LabelNeedsReview = "needs_review"
	LabelDone        = "done"
	LabelAbandoned   = "abandoned"
)

// CanonicalLabels is display order for boards and help text.
var CanonicalLabels = []string{
	LabelBlocked,
	LabelNeedsReview,
	LabelInProgress,
	LabelOpen,
	LabelDone,
	LabelAbandoned,
}

// ParseLabel normalizes user/input text to a canonical label.
// Accepts aliases: in-progress, wip, review, ready, close/closed → abandoned.
func ParseLabel(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	switch s {
	case LabelOpen, "new":
		return LabelOpen, true
	case LabelInProgress, "inprogress", "wip", "progress", "working":
		return LabelInProgress, true
	case LabelBlocked, "block", "stuck":
		return LabelBlocked, true
	case LabelNeedsReview, "needsreview", "review", "ready", "pr_ready":
		return LabelNeedsReview, true
	case LabelDone, "complete", "completed", "merged", "shipped":
		return LabelDone, true
	case LabelAbandoned, "abandon", "closed", "close", "wontfix", "cancelled":
		return LabelAbandoned, true
	default:
		return "", false
	}
}

// EffectiveLabel returns the session label, defaulting empty to open.
func (e Entry) EffectiveLabel() string {
	if lab, ok := ParseLabel(e.Label); ok {
		return lab
	}
	return LabelOpen
}

// IsTerminalLabel reports done or abandoned.
func IsTerminalLabel(label string) bool {
	lab, ok := ParseLabel(label)
	if !ok {
		return false
	}
	return lab == LabelDone || lab == LabelAbandoned
}

// DisplayLabel is a short human form for Discord cards.
func DisplayLabel(label string) string {
	lab, ok := ParseLabel(label)
	if !ok {
		lab = LabelOpen
	}
	switch lab {
	case LabelInProgress:
		return "in progress"
	case LabelNeedsReview:
		return "needs review"
	default:
		return lab
	}
}

// SetLabelManual sets a lifecycle label and marks it manual (auto pauses).
func (e *Entry) SetLabelManual(label string) error {
	if e == nil {
		return fmt.Errorf("nil entry")
	}
	lab, ok := ParseLabel(label)
	if !ok {
		return fmt.Errorf("unknown label %q", label)
	}
	e.Label = lab
	e.LabelManual = true
	return nil
}

// ClearLabelManual re-enables auto transitions and applies SuggestAutoLabel.
func (e *Entry) ClearLabelManual() {
	if e == nil {
		return
	}
	e.LabelManual = false
	e.ApplyAutoLabel(e.SuggestAutoLabel(false))
}

// SuggestAutoLabel derives a label from PR state (and optional running flag).
// Manual lock is ignored here — caller decides whether to apply.
// K18: when Mode=case && Phase=closed, returns current effective label (no PR-driven change).
// When Mode=case and Phase not fixing/shipping, suppress needs_review from stale/open PRs.
func (e Entry) SuggestAutoLabel(running bool) string {
	// K18 close freeze: never suggest a PR-driven change for closed cases.
	if e.IsCaseClosed() {
		return e.EffectiveLabel()
	}

	e.NormalizePRs()
	if len(e.PRs) > 0 && e.AllPRsTerminal() {
		// Open cases (any phase): never force done/abandoned from leftover terminal
		// PRs — reopen after a shipped fix must keep active board labels. Closed
		// cases already returned above. Non-cases still honor PR terminal.
		if e.IsCase() {
			if running {
				return LabelInProgress
			}
			return e.EffectiveLabel()
		}
		for _, p := range e.PRs {
			if strings.EqualFold(strings.TrimSpace(p.State), "MERGED") {
				return LabelDone
			}
		}
		return LabelAbandoned
	}
	if e.HasOpenPR() {
		// Case intake/investigate/answered: do not promote to needs_review from stale PR.
		if e.IsCase() && !e.IsCaseShipPhase() {
			if running {
				return LabelInProgress
			}
			cur := e.EffectiveLabel()
			if cur == LabelOpen {
				return LabelInProgress
			}
			return cur
		}
		// Ready (non-draft) open PR → needs review; draft-only stays in progress.
		for _, p := range e.OpenPRs() {
			if !p.IsDraft {
				return LabelNeedsReview
			}
		}
		return LabelInProgress
	}
	if running {
		return LabelInProgress
	}
	// Work started (session / worktree) but no PR yet.
	if e.SessionID != "" || e.WorktreeBranch != "" {
		return LabelInProgress
	}
	return LabelOpen
}

// ApplyAutoLabel updates Label from a suggestion when allowed.
// Manual labels are sticky except terminal auto (done / abandoned from PRs).
// K18: Mode=case && Phase=closed → no-op (close freezes label without LabelManual).
// Returns true when the stored label changed.
func (e *Entry) ApplyAutoLabel(suggested string) bool {
	if e == nil {
		return false
	}
	// K18: closed cases never accept auto-label (including terminal PR override).
	if e.IsCaseClosed() {
		return false
	}
	lab, ok := ParseLabel(suggested)
	if !ok {
		return false
	}
	cur := e.EffectiveLabel()

	if e.LabelManual {
		// Merge/close still win so the board reflects shipping reality.
		// Exception: closed cases already returned above.
		if lab != LabelDone && lab != LabelAbandoned {
			return false
		}
		// Don't abandon over a manual done.
		if cur == LabelDone && lab == LabelAbandoned {
			return false
		}
	}

	// Don't demote needs_review → in_progress when a follow-up run starts.
	if cur == LabelNeedsReview && lab == LabelInProgress {
		return false
	}
	// Don't revive terminal threads via weak signals.
	if IsTerminalLabel(cur) && !IsTerminalLabel(lab) && !e.LabelManual {
		// Allow revival only when a new open PR appears (needs_review / in_progress from draft).
		if lab != LabelNeedsReview && lab != LabelInProgress {
			return false
		}
		// Only if we still have open PRs (re-opened).
		e.NormalizePRs()
		if !e.HasOpenPR() {
			return false
		}
	}

	if cur == lab && e.Label == lab {
		return false
	}
	e.Label = lab
	if lab == LabelDone || lab == LabelAbandoned {
		// Terminal auto clears the manual lock so the board stays honest.
		e.LabelManual = false
	}
	return true
}

// ApplyAutoLabelOnRunStart sets in_progress from open when a task starts.
// Direct-to-primary sessions also revive terminal done/abandoned → in_progress
// so follow-up tasks on the same thread are not stuck after a ship.
// K18: closed cases do not revive.
func (e *Entry) ApplyAutoLabelOnRunStart() bool {
	if e == nil || e.LabelManual {
		return false
	}
	if e.IsCaseClosed() {
		return false
	}
	switch e.EffectiveLabel() {
	case LabelOpen:
		e.Label = LabelInProgress
		return true
	case LabelDone, LabelAbandoned:
		// PR-mode terminal threads usually get deleted; direct mode keeps the
		// session. Allow revival without an open PR when ShipMode is direct.
		// Case closed already returned; case answered uses blocked not done.
		if e.IsDirectShip() {
			e.Label = LabelInProgress
			return true
		}
		return false
	default:
		return false
	}
}
