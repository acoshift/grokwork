package bot

import (
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPreserveModeAndShipMode(t *testing.T) {
	prev := sessionstore.Entry{
		Mode:          "investigate",
		ShipMode:      sessionstore.ShipModeDirect,
		ShippedSHA:    "abc",
		PrimaryBranch: "main",
		ReopenedAt:    "2026-01-02T00:00:00Z",
		ReopenedBy:    "u-inv",
		Checkpoints: []sessionstore.CheckpointMeta{
			{ID: "c1", SHA: "deadbeef", Ref: "refs/grok-cp/t/c1"},
		},
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q1", Text: "ok?"}},
		VerifyMsgID:   "vm1",
	}
	next := sessionstore.Entry{
		SessionID: "s1",
		Project:   "app",
	}
	preservePRFields(&next, prev)
	if next.Mode != "investigate" {
		t.Fatalf("Mode=%q", next.Mode)
	}
	if next.ShipMode != sessionstore.ShipModeDirect {
		t.Fatalf("ShipMode=%q", next.ShipMode)
	}
	if next.ShippedSHA != "abc" || next.PrimaryBranch != "main" {
		t.Fatalf("ship fields lost: %+v", next)
	}
	if next.ReopenedAt != "2026-01-02T00:00:00Z" || next.ReopenedBy != "u-inv" {
		t.Fatalf("reopen fields lost: at=%q by=%q", next.ReopenedAt, next.ReopenedBy)
	}
	if len(next.Checkpoints) != 1 || next.VerifyMsgID != "vm1" || len(next.OpenQuestions) != 1 {
		t.Fatalf("wave2 fields lost: %+v", next)
	}
	// Explicit next.Mode wins
	next2 := sessionstore.Entry{Mode: "fix"}
	preservePRFields(&next2, prev)
	if next2.Mode != "fix" {
		t.Fatalf("explicit Mode overwritten: %q", next2.Mode)
	}
}
