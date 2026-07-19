package sessionstore

import "testing"

func TestParseLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"open", LabelOpen, true},
		{"in_progress", LabelInProgress, true},
		{"in-progress", LabelInProgress, true},
		{"wip", LabelInProgress, true},
		{"blocked", LabelBlocked, true},
		{"needs_review", LabelNeedsReview, true},
		{"ready", LabelNeedsReview, true},
		{"review", LabelNeedsReview, true},
		{"done", LabelDone, true},
		{"merged", LabelDone, true},
		{"abandoned", LabelAbandoned, true},
		{"close", LabelAbandoned, true},
		{"nope", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseLabel(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%q: got (%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEffectiveLabelDefault(t *testing.T) {
	e := Entry{}
	if e.EffectiveLabel() != LabelOpen {
		t.Fatalf("got %q", e.EffectiveLabel())
	}
	e.Label = "needs_review"
	if e.EffectiveLabel() != LabelNeedsReview {
		t.Fatalf("got %q", e.EffectiveLabel())
	}
}

func TestSuggestAutoLabelFromPRs(t *testing.T) {
	e := Entry{SessionID: "s"}
	if e.SuggestAutoLabel(false) != LabelInProgress {
		t.Fatalf("session without PR: %q", e.SuggestAutoLabel(false))
	}

	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/1", Number: 1, State: "OPEN", IsDraft: true})
	if e.SuggestAutoLabel(false) != LabelInProgress {
		t.Fatalf("draft: %q", e.SuggestAutoLabel(false))
	}

	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/1", Number: 1, State: "OPEN", IsDraft: false})
	if e.SuggestAutoLabel(false) != LabelNeedsReview {
		t.Fatalf("ready PR: %q", e.SuggestAutoLabel(false))
	}

	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/1", Number: 1, State: "MERGED"})
	if e.SuggestAutoLabel(false) != LabelDone {
		t.Fatalf("merged: %q", e.SuggestAutoLabel(false))
	}

	e2 := Entry{}
	e2.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/2", Number: 2, State: "CLOSED"})
	if e2.SuggestAutoLabel(false) != LabelAbandoned {
		t.Fatalf("closed: %q", e2.SuggestAutoLabel(false))
	}

	e3 := Entry{}
	if e3.SuggestAutoLabel(true) != LabelInProgress {
		t.Fatalf("running: %q", e3.SuggestAutoLabel(true))
	}
	if e3.SuggestAutoLabel(false) != LabelOpen {
		t.Fatalf("idle empty: %q", e3.SuggestAutoLabel(false))
	}
}

func TestApplyAutoLabelManualSticky(t *testing.T) {
	e := Entry{Label: LabelBlocked, LabelManual: true}
	if e.ApplyAutoLabel(LabelInProgress) {
		t.Fatal("manual blocked should not become in_progress")
	}
	if e.EffectiveLabel() != LabelBlocked {
		t.Fatalf("label=%q", e.EffectiveLabel())
	}
	// Terminal auto still wins.
	if !e.ApplyAutoLabel(LabelDone) {
		t.Fatal("expected done override")
	}
	if e.EffectiveLabel() != LabelDone || e.LabelManual {
		t.Fatalf("after done: %+v", e)
	}
}

func TestApplyAutoLabelNoDemoteNeedsReview(t *testing.T) {
	e := Entry{Label: LabelNeedsReview}
	if e.ApplyAutoLabel(LabelInProgress) {
		t.Fatal("should not demote needs_review")
	}
}

func TestApplyAutoLabelOnRunStart(t *testing.T) {
	e := Entry{}
	if !e.ApplyAutoLabelOnRunStart() || e.Label != LabelInProgress {
		t.Fatalf("open→in_progress: %+v", e)
	}
	e.Label = LabelNeedsReview
	if e.ApplyAutoLabelOnRunStart() {
		t.Fatal("should not touch needs_review")
	}
	e.Label = LabelOpen
	e.LabelManual = true
	if e.ApplyAutoLabelOnRunStart() {
		t.Fatal("manual should block")
	}
}

func TestSetLabelManualAndClear(t *testing.T) {
	e := Entry{SessionID: "s"}
	if err := e.SetLabelManual("blocked"); err != nil {
		t.Fatal(err)
	}
	if e.Label != LabelBlocked || !e.LabelManual {
		t.Fatalf("%+v", e)
	}
	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/1", Number: 1, State: "OPEN", IsDraft: false})
	e.ClearLabelManual()
	if e.LabelManual || e.Label != LabelNeedsReview {
		t.Fatalf("auto after clear: %+v", e)
	}
}

func TestDisplayLabel(t *testing.T) {
	if DisplayLabel(LabelNeedsReview) != "needs review" {
		t.Fatal(DisplayLabel(LabelNeedsReview))
	}
	if DisplayLabel("") != "open" {
		t.Fatal(DisplayLabel(""))
	}
}
