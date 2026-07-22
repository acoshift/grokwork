package sessionstore

import "testing"

func TestSuggestAutoLabelClosedCaseFrozen(t *testing.T) {
	e := Entry{
		Mode:     "case",
		Phase:    PhaseClosed,
		Label:    LabelAbandoned,
		PRState:  "MERGED",
		PRNumber: 1,
		PRs: []TrackedPR{{
			Number: 1, State: "MERGED", URL: "https://example/pr/1",
		}},
	}
	// Would normally suggest done from MERGED
	got := e.SuggestAutoLabel(false)
	if got != LabelAbandoned {
		t.Fatalf("SuggestAutoLabel on closed case: got %q want abandoned", got)
	}
	if e.ApplyAutoLabel(LabelDone) {
		t.Fatal("ApplyAutoLabel must no-op on closed case")
	}
	if e.Label != LabelAbandoned {
		t.Fatalf("label changed to %q", e.Label)
	}
}

func TestSuggestAutoLabelCaseInvestigateNoNeedsReview(t *testing.T) {
	e := Entry{
		Mode:  "case",
		Phase: PhaseInvestigate,
		Label: LabelInProgress,
		PRs: []TrackedPR{{
			Number: 9, State: "OPEN", IsDraft: false, URL: "https://example/pr/9",
		}},
	}
	got := e.SuggestAutoLabel(false)
	if got == LabelNeedsReview {
		t.Fatal("case investigate must not promote needs_review from stale PR")
	}
	if got != LabelInProgress {
		t.Fatalf("got %q", got)
	}
}

func TestSuggestAutoLabelCaseFixingNeedsReview(t *testing.T) {
	e := Entry{
		Mode:  "case",
		Phase: PhaseFixing,
		Label: LabelInProgress,
		PRs: []TrackedPR{{
			Number: 9, State: "OPEN", IsDraft: false, URL: "https://example/pr/9",
		}},
	}
	got := e.SuggestAutoLabel(false)
	if got != LabelNeedsReview {
		t.Fatalf("case fixing open PR: got %q", got)
	}
}

func TestClampCaseFields(t *testing.T) {
	long := string(make([]byte, 3000))
	for i := range long {
		long = long[:i] + "a" + long[i+1:]
	}
	// simpler
	r := make([]rune, 2500)
	for i := range r {
		r[i] = 'x'
	}
	e := Entry{CustomerUpdate: string(r), CustomerTitle: string(r[:300])}
	if err := ClampCaseFields(&e); err != nil {
		t.Fatal(err)
	}
	if len([]rune(e.CustomerUpdate)) > maxCustomerUpdateRunes {
		t.Fatalf("CustomerUpdate too long: %d", len([]rune(e.CustomerUpdate)))
	}
}
