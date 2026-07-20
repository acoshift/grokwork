package ghpr

import (
	"strings"
	"testing"
)

func TestDiffTimelineFirstSeedOnlyTerminal(t *testing.T) {
	// First observation of an open approved PR: no spam.
	ev := DiffTimeline(Snapshot{}, Snapshot{
		State:  "OPEN",
		Review: "APPROVED",
		Checks: "✓ 3",
	})
	if len(ev) != 0 {
		t.Fatalf("first open seed should be quiet, got %+v", ev)
	}

	// Already merged when first seen: announce.
	ev = DiffTimeline(Snapshot{}, Snapshot{State: "MERGED"})
	if len(ev) != 1 || ev[0].Kind != TimelineMerged {
		t.Fatalf("want merged on first terminal, got %+v", ev)
	}
}

func TestDiffTimelineReviewTransitions(t *testing.T) {
	prev := Snapshot{State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "✓ 1"}
	ev := DiffTimeline(prev, Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 1"})
	if len(ev) != 1 || ev[0].Kind != TimelineApproved {
		t.Fatalf("approved: %+v", ev)
	}

	ev = DiffTimeline(
		Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 1"},
		Snapshot{State: "OPEN", Review: "CHANGES_REQUESTED", Checks: "✓ 1"},
	)
	if len(ev) != 1 || ev[0].Kind != TimelineChangesRequested {
		t.Fatalf("changes: %+v", ev)
	}

	// REVIEW_REQUIRED is not an event (noise).
	ev = DiffTimeline(
		Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 1"},
		Snapshot{State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "✓ 1"},
	)
	if len(ev) != 0 {
		t.Fatalf("review_required should be quiet: %+v", ev)
	}
}

func TestDiffTimelineCIGreen(t *testing.T) {
	prev := Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 2 · ✗ 1"}
	next := Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 3"}
	ev := DiffTimeline(prev, next)
	if len(ev) != 1 || ev[0].Kind != TimelineCIGreen {
		t.Fatalf("ci green from fail: %+v", ev)
	}
	if ev[0].Detail != "✓ 3" {
		t.Fatalf("detail=%q", ev[0].Detail)
	}

	// Pending → green.
	ev = DiffTimeline(
		Snapshot{State: "OPEN", Checks: "✓ 1 · … 2"},
		Snapshot{State: "OPEN", Checks: "✓ 3"},
	)
	if len(ev) != 1 || ev[0].Kind != TimelineCIGreen {
		t.Fatalf("ci green from pending: %+v", ev)
	}

	// Already green → still green: quiet.
	ev = DiffTimeline(
		Snapshot{State: "OPEN", Checks: "✓ 3"},
		Snapshot{State: "OPEN", Checks: "✓ 3"},
	)
	if len(ev) != 0 {
		t.Fatalf("stable green: %+v", ev)
	}

	// Empty → green is first-ish checks seed after URL-only track with State set.
	// prev has state but empty checks — no prior CI signal.
	ev = DiffTimeline(
		Snapshot{State: "OPEN", Checks: ""},
		Snapshot{State: "OPEN", Checks: "✓ 2"},
	)
	if len(ev) != 0 {
		t.Fatalf("no prior CI signal: %+v", ev)
	}

	// Terminal PR does not announce CI green.
	ev = DiffTimeline(
		Snapshot{State: "OPEN", Checks: "✗ 1"},
		Snapshot{State: "MERGED", Checks: "✓ 1"},
	)
	if len(ev) != 1 || ev[0].Kind != TimelineMerged {
		t.Fatalf("merged only: %+v", ev)
	}
}

func TestDiffTimelineMultiAndClosed(t *testing.T) {
	// Approve + CI green in same poll.
	ev := DiffTimeline(
		Snapshot{State: "OPEN", Review: "REVIEW_REQUIRED", Checks: "… 2"},
		Snapshot{State: "OPEN", Review: "APPROVED", Checks: "✓ 2"},
	)
	if len(ev) != 2 {
		t.Fatalf("want 2 events, got %+v", ev)
	}
	if ev[0].Kind != TimelineApproved || ev[1].Kind != TimelineCIGreen {
		t.Fatalf("order: %+v", ev)
	}

	ev = DiffTimeline(
		Snapshot{State: "OPEN"},
		Snapshot{State: "CLOSED"},
	)
	if len(ev) != 1 || ev[0].Kind != TimelineClosed {
		t.Fatalf("closed: %+v", ev)
	}
}

func TestChecksAllGreen(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"✓ 3", true},
		{"✓ 2 · · 1", true}, // other/skip only
		{"✓ 2 · ✗ 1", false},
		{"✓ 1 · … 1", false},
		{"none", false},
		{"", false},
		{"✗ 1", false},
	}
	for _, tc := range cases {
		if got := ChecksAllGreen(tc.in); got != tc.want {
			t.Errorf("ChecksAllGreen(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestFormatTimeline(t *testing.T) {
	info := Info{
		Number: 12,
		Owner:  "acoshift",
		Repo:   "grokwork",
		URL:    "https://github.com/acoshift/grokwork/pull/12",
	}
	msg := FormatTimeline(info, []TimelineEvent{
		{Kind: TimelineApproved},
		{Kind: TimelineCIGreen, Detail: "✓ 4"},
	})
	for _, want := range []string{
		"**PR event**",
		"acoshift/grokwork#12",
		"APPROVED",
		"CI: **green**",
		"✓ 4",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
	// Non-terminal: no URL line.
	if strings.Contains(msg, "https://") {
		t.Fatalf("unexpected URL on non-terminal:\n%s", msg)
	}

	merged := FormatTimeline(info, []TimelineEvent{{Kind: TimelineMerged}})
	if !strings.Contains(merged, "MERGED") || !strings.Contains(merged, info.URL) {
		t.Fatalf("merged msg:\n%s", merged)
	}
	if !HasTerminalTimeline([]TimelineEvent{{Kind: TimelineMerged}}) {
		t.Fatal("HasTerminalTimeline")
	}
	if HasTerminalTimeline([]TimelineEvent{{Kind: TimelineApproved}}) {
		t.Fatal("approved is not terminal")
	}
}

func TestFormatTimelineEmbed(t *testing.T) {
	info := Info{
		Number: 12,
		Owner:  "acoshift",
		Repo:   "grokwork",
		URL:    "https://github.com/acoshift/grokwork/pull/12",
	}

	if _, ok := FormatTimelineEmbed(info, nil); ok {
		t.Fatal("empty events should not be ok")
	}

	emb, ok := FormatTimelineEmbed(info, []TimelineEvent{
		{Kind: TimelineApproved},
		{Kind: TimelineCIGreen, Detail: "✓ 4"},
	})
	if !ok {
		t.Fatal("expected embed")
	}
	if emb.Title != "PR event · acoshift/grokwork#12" {
		t.Fatalf("title=%q", emb.Title)
	}
	if emb.URL != info.URL {
		t.Fatalf("url=%q", emb.URL)
	}
	if emb.Color != timelineColorSuccess {
		t.Fatalf("color=%#x want success %#x", emb.Color, timelineColorSuccess)
	}
	for _, want := range []string{"APPROVED", "CI: **green**", "✓ 4"} {
		if !strings.Contains(emb.Description, want) {
			t.Fatalf("missing %q in desc:\n%s", want, emb.Description)
		}
	}

	merged, ok := FormatTimelineEmbed(info, []TimelineEvent{{Kind: TimelineMerged}})
	if !ok || merged.Color != timelineColorMerged {
		t.Fatalf("merged embed: ok=%v color=%#x", ok, merged.Color)
	}
	closed, ok := FormatTimelineEmbed(info, []TimelineEvent{{Kind: TimelineClosed}})
	if !ok || closed.Color != timelineColorClosed {
		t.Fatalf("closed embed: ok=%v color=%#x", ok, closed.Color)
	}
	changes, ok := FormatTimelineEmbed(info, []TimelineEvent{{Kind: TimelineChangesRequested}})
	if !ok || changes.Color != timelineColorChanges {
		t.Fatalf("changes embed: ok=%v color=%#x", ok, changes.Color)
	}

	// Multi-event: merged wins color priority over success.
	mixed, ok := FormatTimelineEmbed(info, []TimelineEvent{
		{Kind: TimelineApproved},
		{Kind: TimelineMerged},
	})
	if !ok || mixed.Color != timelineColorMerged {
		t.Fatalf("mixed color=%#x want merged", mixed.Color)
	}
}

func TestSnapshotFromInfo(t *testing.T) {
	s := SnapshotFromInfo(Info{
		State: "OPEN", ReviewDecision: "APPROVED", Checks: "✓ 1",
	})
	if s.State != "OPEN" || s.Review != "APPROVED" || s.Checks != "✓ 1" {
		t.Fatalf("%+v", s)
	}
}
