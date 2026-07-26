package bot

import (
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestPreserveModeFieldsKeepsCaseIdentity pins the case's public id against
// Set rebuilds. Several paths (PR status refresh, ship stamping) reconstruct an
// Entry from a partial and hand it to Set; preserveModeFields is what stops
// those from erasing the case record. CaseKey is the one field where loss is
// unrecoverable — it is what commits, PRs and other cases point at, so a
// rebuild that drops it silently breaks references that are already written
// down somewhere this process cannot see.
func TestPreserveModeFieldsKeepsCaseIdentity(t *testing.T) {
	prev := sessionstore.Entry{
		Mode:          "case",
		Phase:         sessionstore.PhaseFixing,
		Severity:      "high",
		CustomerTitle: "Checkout 500s",
		CaseKey:       "WEBAPP-14",
		RelatedCases:  []string{"WEBAPP-9", "WEBAPP-3"},
	}
	next := sessionstore.Entry{Project: "webapp"} // a rebuild carrying none of it
	preserveModeFields(&next, prev)

	if next.CaseKey != "WEBAPP-14" {
		t.Fatalf("CaseKey = %q, want WEBAPP-14", next.CaseKey)
	}
	if got := next.RelatedCaseKeys(); len(got) != 2 || got[0] != "WEBAPP-9" || got[1] != "WEBAPP-3" {
		t.Fatalf("RelatedCases = %v", got)
	}
	// Copied, not aliased: mutating the rebuilt entry must not reach back into
	// the value the caller still holds.
	next.RelatedCases[0] = "MUTATED-1"
	if prev.RelatedCases[0] != "WEBAPP-9" {
		t.Fatal("preserved slice aliases the previous entry")
	}

	// An explicit value on the rebuild wins — preserve fills gaps, it does not
	// overwrite. (Nothing re-keys a case today; this is the guarantee that a
	// future migration could.)
	next2 := sessionstore.Entry{CaseKey: "WEBAPP-99", RelatedCases: []string{"WEBAPP-1"}}
	preserveModeFields(&next2, prev)
	if next2.CaseKey != "WEBAPP-99" || len(next2.RelatedCases) != 1 {
		t.Fatalf("preserve overwrote an explicit value: %q %v", next2.CaseKey, next2.RelatedCases)
	}
}
