package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func TestParseLabelArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/label", ""},
		{"label", ""},
		{"/label blocked", "blocked"},
		{"/label auto", "auto"},
		{"/label needs_review", "needs_review"},
	}
	for _, tc := range cases {
		if got := parseLabelArg(tc.in); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseBoardArgs(t *testing.T) {
	projects := map[string]struct{}{"homeconnect": {}, "api": {}}

	proj, lab, all, errMsg := parseBoardArgs("/board", projects)
	if proj != "" || lab != "" || all || errMsg != "" {
		t.Fatalf("empty: %q %q %v %q", proj, lab, all, errMsg)
	}

	proj, lab, all, errMsg = parseBoardArgs("/board needs_review", projects)
	if proj != "" || lab != sessionstore.LabelNeedsReview || all || errMsg != "" {
		t.Fatalf("label: %q %q %v %q", proj, lab, all, errMsg)
	}

	proj, lab, all, errMsg = parseBoardArgs("/board homeconnect blocked", projects)
	if proj != "homeconnect" || lab != sessionstore.LabelBlocked || all || errMsg != "" {
		t.Fatalf("proj+label: %q %q %v %q", proj, lab, all, errMsg)
	}

	proj, lab, all, errMsg = parseBoardArgs("/board all", projects)
	if !all || errMsg != "" {
		t.Fatalf("all: %v %q", all, errMsg)
	}

	proj, lab, all, errMsg = parseBoardArgs("/board done", projects)
	if lab != sessionstore.LabelDone || !all || errMsg != "" {
		t.Fatalf("done implies terminal: %q %v %q", lab, all, errMsg)
	}

	_, _, _, errMsg = parseBoardArgs("/board nope", projects)
	if errMsg == "" {
		t.Fatal("expected error for unknown filter")
	}
}

func TestFormatBoardCard(t *testing.T) {
	rows := []boardRow{
		{ThreadID: "1", Project: "api", Label: sessionstore.LabelNeedsReview, Goal: "ship rate limit", OwnerID: "u1"},
		{ThreadID: "2", Project: "api", Label: sessionstore.LabelInProgress, Goal: "fix flaky", Running: true},
		{ThreadID: "3", Project: "web", Label: sessionstore.LabelBlocked, Goal: "waiting on design"},
	}
	card := formatBoardCard(rows, "", "", false)
	for _, want := range []string{
		"**Board**",
		"**blocked** (1)",
		"**needs review** (1)",
		"**in progress** (1)",
		"<#1>",
		"**api**",
		"_running_",
		"<@u1>",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("missing %q in:\n%s", want, card)
		}
	}
	// Canonical order: blocked before needs_review before in_progress.
	iBlock := strings.Index(card, "**blocked**")
	iReview := strings.Index(card, "**needs review**")
	iProg := strings.Index(card, "**in progress**")
	if !(iBlock < iReview && iReview < iProg) {
		t.Fatalf("order wrong:\n%s", card)
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

func TestFormatBriefCardIncludesLabel(t *testing.T) {
	card := FormatBriefCard(BriefCardInput{
		Project:   "app",
		Goal:      "x",
		Label:     "needs review",
		LabelMode: "manual",
	})
	if !strings.Contains(card, "**label:** needs review (manual)") {
		t.Fatalf("%s", card)
	}
}
