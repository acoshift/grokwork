package sessionstore

import (
	"strings"
	"time"
)

// SLA clocks on a case.
//
// This file owns the timestamps and nothing else. The targets they are measured
// against are per-project policy (config.SLATarget), and whether a clock is
// breached is computed where it is rendered (internal/bot/case_sla.go).
//
// A *round* is one open period of a case: from intake — or from the last reopen —
// until the case is closed. Reopening starts a new round rather than resuming
// the old one, because a customer who came back is waiting again, and the reply
// they already got answers nothing about the wait that just started.

// ParseStamp reads one of Entry's RFC3339 timestamps. ok is false for an empty
// or unparseable value: these fields are hand-editable JSON, and a zero time
// would silently read as January 1st year 1 — a start that breaches everything
// and a stop that breaches nothing.
func ParseStamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func formatStamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// SLARoundStart is the origin of the case's current SLA round: the last reopen
// when there was one, otherwise intake.
//
// ok is false when the case carries neither. Cases filed before these stamps
// existed have no start, and deriving one from UpdatedAt would breach the whole
// archive at once on the first deploy — an SLA needs a start it can defend.
func (e Entry) SLARoundStart() (time.Time, bool) {
	opened, okOpened := ParseStamp(e.OpenedAt)
	reopened, okReopened := ParseStamp(e.ReopenedAt)
	switch {
	case okOpened && okReopened:
		if reopened.After(opened) {
			return reopened, true
		}
		return opened, true
	case okReopened:
		return reopened, true
	case okOpened:
		return opened, true
	default:
		return time.Time{}, false
	}
}

// MarkCaseOpened stamps the SLA round origin when a case is filed.
//
// Never overwrites: /case run a second time on the same thread is a re-file with
// a corrected title, not a new incident, and re-stamping would quietly forgive
// however long the case had already been waiting.
func MarkCaseOpened(e *Entry, now time.Time) {
	if e == nil || strings.TrimSpace(e.OpenedAt) != "" {
		return
	}
	e.OpenedAt = formatStamp(now)
}

// MarkCaseResponded records the first customer-facing response of the current
// round — a customer update, or /answer.
//
// Only the first counts; a second update is not a faster first response.
// Internal activity deliberately does not count: an investigate run answers us,
// not them, and neither does an escalation.
func MarkCaseResponded(e *Entry, now time.Time) {
	if e == nil || strings.TrimSpace(e.FirstResponseAt) != "" {
		return
	}
	e.FirstResponseAt = formatStamp(now)
}

// MarkCaseWaitingOnCustomer records handing the case back to the customer
// (phase answered), which is also a response.
//
// Unlike FirstResponseAt this is overwritten every time: it is the instant the
// resolution clock freezes at while we wait, so the most recent handoff is the
// one that matters.
func MarkCaseWaitingOnCustomer(e *Entry, now time.Time) {
	if e == nil {
		return
	}
	MarkCaseResponded(e, now)
	e.AnsweredAt = formatStamp(now)
}

// ResetCaseSLARound clears the current round's response and hold stamps. Called
// on reopen, where ReopenedAt becomes the new round's origin (SLARoundStart):
// the case has to be responded to again, and a stale AnsweredAt would otherwise
// sit before the round it is supposed to freeze.
func ResetCaseSLARound(e *Entry) {
	if e == nil {
		return
	}
	e.FirstResponseAt = ""
	e.AnsweredAt = ""
}
