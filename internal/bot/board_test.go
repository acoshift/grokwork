package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParseBoardArgs(t *testing.T) {
	lab, act, all, errMsg := parseBoardArgs("/board")
	if lab != "" || act != "" || all || errMsg != "" {
		t.Fatalf("empty: %q %q %v %q", lab, act, all, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board needs_review")
	if lab != sessionstore.LabelNeedsReview || act != "" || all || errMsg != "" {
		t.Fatalf("label: %q %q %v %q", lab, act, all, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board blocked")
	if lab != sessionstore.LabelBlocked || act != "" || all || errMsg != "" {
		t.Fatalf("label blocked: %q %q %v %q", lab, act, all, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board all")
	if !all || errMsg != "" || act != "" {
		t.Fatalf("all: %v %q %q", all, act, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board done")
	// "done" is an activity filter (and implies terminal).
	if lab != "" || act != activityDone || !all || errMsg != "" {
		t.Fatalf("done activity: lab=%q act=%q all=%v err=%q", lab, act, all, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board waiting")
	if act != activityWaiting || lab != "" || all || errMsg != "" {
		t.Fatalf("waiting: act=%q lab=%q all=%v err=%q", act, lab, all, errMsg)
	}

	lab, act, all, errMsg = parseBoardArgs("/board stale")
	if act != activityStale || lab != "" || errMsg != "" {
		t.Fatalf("stale: act=%q lab=%q err=%q", act, lab, errMsg)
	}

	// Project names are not board filters; scope comes from the channel mapping.
	_, _, _, errMsg = parseBoardArgs("/board homeconnect")
	if errMsg == "" {
		t.Fatal("expected error for project name filter")
	}

	_, _, _, errMsg = parseBoardArgs("/board nope")
	if errMsg == "" {
		t.Fatal("expected error for unknown filter")
	}
}

func TestClassifyActivity(t *testing.T) {
	cases := []struct {
		name      string
		running   bool
		queue     int
		wait      string
		label     string
		idleDays  int
		staleDays int
		want      string
	}{
		{"running wins over wait", true, 1, "blocked", sessionstore.LabelBlocked, 10, 3, activityRunning},
		{"queued without job", false, 2, "", sessionstore.LabelInProgress, 0, 3, activityQueued},
		{"waiting blocked", false, 0, "blocked", sessionstore.LabelBlocked, 0, 3, activityWaiting},
		{"stale", false, 0, "", sessionstore.LabelInProgress, 5, 3, activityStale},
		{"active fresh", false, 0, "", sessionstore.LabelInProgress, 1, 3, activityActive},
		{"done terminal", false, 0, "", sessionstore.LabelDone, 99, 3, activityDone},
		{"abandoned terminal", false, 0, "", sessionstore.LabelAbandoned, 0, 3, activityAbandoned},
		{"waiting beats stale", false, 0, "needs review", sessionstore.LabelNeedsReview, 30, 3, activityWaiting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyActivity(tc.running, tc.queue, tc.wait, tc.label, tc.idleDays, tc.staleDays)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestWaitingOnHumanReason(t *testing.T) {
	if got := waitingOnHumanReason(sessionstore.Entry{Label: sessionstore.LabelBlocked}); got != "blocked" {
		t.Fatalf("blocked: %q", got)
	}
	e := sessionstore.Entry{
		Label: sessionstore.LabelNeedsReview,
		PRs: []sessionstore.TrackedPR{{
			Number: 1, State: "OPEN", Review: "CHANGES_REQUESTED",
		}},
	}
	if got := waitingOnHumanReason(e); got != "changes requested" {
		t.Fatalf("changes: %q", got)
	}
	e2 := sessionstore.Entry{
		Label: sessionstore.LabelInProgress,
		PRs: []sessionstore.TrackedPR{{
			Number: 2, State: "OPEN", Checks: "✓ 1 · ✗ 2",
		}},
	}
	if got := waitingOnHumanReason(e2); got != "CI failing" {
		t.Fatalf("ci: %q", got)
	}
	e3 := sessionstore.Entry{Label: sessionstore.LabelNeedsReview}
	if got := waitingOnHumanReason(e3); got != "needs review" {
		t.Fatalf("review: %q", got)
	}
	if got := waitingOnHumanReason(sessionstore.Entry{Label: sessionstore.LabelInProgress}); got != "" {
		t.Fatalf("idle in progress: %q", got)
	}
}

func TestFormatBoardCardActivity(t *testing.T) {
	rows := []boardRow{
		{ThreadID: "1", Project: "api", Label: sessionstore.LabelNeedsReview, Goal: "ship rate limit", OwnerID: "u1", Activity: activityWaiting, WaitReason: "needs review"},
		{ThreadID: "2", Project: "api", Label: sessionstore.LabelInProgress, Goal: "fix flaky", Running: true, Activity: activityRunning},
		{ThreadID: "3", Project: "web", Label: sessionstore.LabelBlocked, Goal: "waiting on design", Activity: activityWaiting, WaitReason: "blocked"},
		{ThreadID: "4", Project: "api", Label: sessionstore.LabelInProgress, Goal: "old work", Activity: activityStale, IdleDays: 5},
	}
	card := formatBoardCard(rows, "", "", "", false, 3)
	for _, want := range []string{
		"**Board**",
		"activity",
		"1 running · 2 waiting · 1 stale · 0 active",
		"**running** (1)",
		"**waiting on human** (2)",
		"**stale (≥3d)** (1)",
		"<#1>",
		"**api**",
		"_running_",
		"_needs review_",
		"_blocked_",
		"idle 5d",
		"<@u1>",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("missing %q in:\n%s", want, card)
		}
	}
	// Order: running before waiting before stale.
	iRun := strings.Index(card, "**running**")
	iWait := strings.Index(card, "**waiting on human**")
	iStale := strings.Index(card, "**stale")
	if !(iRun < iWait && iWait < iStale) {
		t.Fatalf("order wrong:\n%s", card)
	}
}

func TestSortBoardRowsPrefersAttention(t *testing.T) {
	rows := []boardRow{
		{ThreadID: "old-active", Activity: activityActive, UpdatedAt: "2026-07-18T00:00:00Z"},
		{ThreadID: "stale", Activity: activityStale, UpdatedAt: "2026-07-01T00:00:00Z"},
		{ThreadID: "wait", Activity: activityWaiting, UpdatedAt: "2026-07-10T00:00:00Z"},
		{ThreadID: "run", Activity: activityRunning, UpdatedAt: "2026-07-19T00:00:00Z"},
	}
	sortBoardRows(rows)
	want := []string{"run", "wait", "stale", "old-active"}
	for i, id := range want {
		if rows[i].ThreadID != id {
			t.Fatalf("pos %d: got %q want %q", i, rows[i].ThreadID, id)
		}
	}
}

func TestIdleWholeDays(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if n := idleWholeDays(now.Add(-50*time.Hour).Format(time.RFC3339), now); n != 2 {
		t.Fatalf("50h → %d want 2", n)
	}
	if n := idleWholeDays("", now); n != 0 {
		t.Fatalf("empty: %d", n)
	}
	if n := idleWholeDays(now.Add(-2*time.Hour).Format(time.RFC3339), now); n != 0 {
		t.Fatalf("2h: %d", n)
	}
}

func TestPreserveLabelFields(t *testing.T) {
	prev := sessionstore.Entry{Label: sessionstore.LabelBlocked, LabelManual: true}
	next := sessionstore.Entry{SessionID: "s"}
	preserveLabelFields(&next, prev)
	if next.Label != sessionstore.LabelBlocked || !next.LabelManual {
		t.Fatalf("%+v", next)
	}
	// Explicit label on next is kept.
	next2 := sessionstore.Entry{Label: sessionstore.LabelDone}
	preserveLabelFields(&next2, prev)
	if next2.Label != sessionstore.LabelDone {
		t.Fatalf("%+v", next2)
	}
}
