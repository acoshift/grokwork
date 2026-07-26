package sessionstore

import (
	"testing"
	"time"
)

func TestParseStampRejectsUnusableValues(t *testing.T) {
	for _, in := range []string{"", "   ", "yesterday", "2026-07-27"} {
		if _, ok := ParseStamp(in); ok {
			t.Fatalf("ParseStamp(%q) accepted a value it cannot use", in)
		}
	}
	got, ok := ParseStamp(" 2026-07-27T10:00:00Z ")
	if !ok || !got.Equal(time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("ParseStamp = %v %v", got, ok)
	}
}

// The round origin is the reopen when there is one: a customer who came back is
// waiting from the reopen, not from the original intake months ago.
func TestSLARoundStart(t *testing.T) {
	opened := "2026-07-01T09:00:00Z"
	reopened := "2026-07-20T09:00:00Z"

	if _, ok := (Entry{}).SLARoundStart(); ok {
		t.Fatal("a case with no stamps must have no round start")
	}
	got, ok := Entry{OpenedAt: opened}.SLARoundStart()
	if !ok || got.Format(time.RFC3339) != opened {
		t.Fatalf("intake start = %v %v", got, ok)
	}
	got, ok = Entry{OpenedAt: opened, ReopenedAt: reopened}.SLARoundStart()
	if !ok || got.Format(time.RFC3339) != reopened {
		t.Fatalf("reopen start = %v %v", got, ok)
	}
	// Hand-edited data where the reopen predates intake must not move the clock
	// backwards.
	got, ok = Entry{OpenedAt: reopened, ReopenedAt: opened}.SLARoundStart()
	if !ok || got.Format(time.RFC3339) != reopened {
		t.Fatalf("out-of-order start = %v %v", got, ok)
	}
	// Unparseable stamps are not a start.
	if _, ok := (Entry{OpenedAt: "soon"}).SLARoundStart(); ok {
		t.Fatal("garbage stamp accepted as a round start")
	}
}

func TestCaseSLAStamps(t *testing.T) {
	t0 := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)

	e := Entry{}
	MarkCaseOpened(&e, t0)
	if e.OpenedAt != "2026-07-27T09:00:00Z" {
		t.Fatalf("OpenedAt = %q", e.OpenedAt)
	}
	// Re-filing a case must not forgive the wait already spent.
	MarkCaseOpened(&e, t0.Add(3*time.Hour))
	if e.OpenedAt != "2026-07-27T09:00:00Z" {
		t.Fatalf("OpenedAt re-stamped: %q", e.OpenedAt)
	}

	MarkCaseResponded(&e, t0.Add(20*time.Minute))
	MarkCaseResponded(&e, t0.Add(50*time.Minute))
	if e.FirstResponseAt != "2026-07-27T09:20:00Z" {
		t.Fatalf("FirstResponseAt = %q (only the first response counts)", e.FirstResponseAt)
	}

	// Answering holds the resolution clock; a second answer moves the hold but
	// not the first response.
	MarkCaseWaitingOnCustomer(&e, t0.Add(2*time.Hour))
	if e.AnsweredAt != "2026-07-27T11:00:00Z" || e.FirstResponseAt != "2026-07-27T09:20:00Z" {
		t.Fatalf("answered=%q firstResponse=%q", e.AnsweredAt, e.FirstResponseAt)
	}
	MarkCaseWaitingOnCustomer(&e, t0.Add(5*time.Hour))
	if e.AnsweredAt != "2026-07-27T14:00:00Z" {
		t.Fatalf("AnsweredAt must track the latest handoff: %q", e.AnsweredAt)
	}

	// A case answered without any earlier update still counts as responded.
	fresh := Entry{}
	MarkCaseWaitingOnCustomer(&fresh, t0)
	if fresh.FirstResponseAt == "" {
		t.Fatal("answering is a response")
	}

	// Reopen clears the round, keeping intake (SLARoundStart prefers ReopenedAt).
	ResetCaseSLARound(&e)
	if e.FirstResponseAt != "" || e.AnsweredAt != "" {
		t.Fatalf("round not reset: %+v", e)
	}
	if e.OpenedAt == "" {
		t.Fatal("reset must not erase when the case was filed")
	}

	// Nil is a no-op rather than a panic: callers stamp inside Patch closures.
	MarkCaseOpened(nil, t0)
	MarkCaseResponded(nil, t0)
	MarkCaseWaitingOnCustomer(nil, t0)
	ResetCaseSLARound(nil)
}
