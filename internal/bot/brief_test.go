package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func TestFormatBriefCardMinimal(t *testing.T) {
	card := FormatBriefCard(BriefCardInput{
		Project: "app",
		Goal:    "fix checkout timeout",
	})
	if !strings.Contains(card, "**Brief** · **app**") {
		t.Fatalf("header: %q", card)
	}
	if !strings.Contains(card, "**goal:** fix checkout timeout") {
		t.Fatalf("goal: %q", card)
	}
	if !strings.Contains(card, "**done:** (none yet)") {
		t.Fatalf("done: %q", card)
	}
	if !strings.Contains(card, "**pr:** (none yet)") {
		t.Fatalf("pr: %q", card)
	}
}

func TestFormatBriefCardFull(t *testing.T) {
	card := FormatBriefCard(BriefCardInput{
		Project:   "api",
		OwnerID:   "u1",
		OwnerName: "alice",
		Goal:      "ship rate limiter",
		Status:    "idle",
		Turns:     2,
		Done:      []string{"investigate (done)", "implement (done)"},
		Left:      "PR open — review/merge",
		Branch:    "grok/discord/1",
		HeadShort: "abc1234",
		PRLines:   []string{"**pr:** #9 · OPEN · https://example.com/p/9"},
		Files:     []string{"M\tapi/rate.go", "A\tapi/limit_test.go"},
		Questions: []string{"Should we use Redis?"},
		Queue:     1,
	})
	for _, want := range []string{
		"**owner:** alice (<@u1>)",
		"**status:** idle · 2 turns",
		"• investigate (done)",
		"**left:** PR open — review/merge",
		"**branch:** `grok/discord/1` @ `abc1234`",
		"**pr:** #9",
		"M api/rate.go",
		"**questions:**",
		"Should we use Redis?",
		"**queue:** 1 follow-up",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("missing %q in:\n%s", want, card)
		}
	}
}

func TestParseBriefGoalArg(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/brief", "", false},
		{"brief", "", false},
		{"/brief goal fix flaky test", "fix flaky test", true},
		{"/brief set goal ship auth", "ship auth", true},
		{"brief goal x", "x", true},
		{"/brief something else", "", false},
		{"/brief goal", "", true}, // empty goal — caller errors
	}
	for _, tc := range cases {
		got, ok := parseBriefGoalArg(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("%q: got (%q, %v) want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestBriefDoneFromHistory(t *testing.T) {
	th := history.Thread{Turns: []history.Turn{
		{Prompt: "first", Status: "done"},
		{Prompt: "cancelled one", Status: "cancelled"},
		{Prompt: "second", Status: "error"},
		{Prompt: "third", Status: "done"},
	}}
	got := briefDoneFromHistory(th)
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "first") || !strings.Contains(got[2], "third") {
		t.Fatalf("order: %v", got)
	}
	if !strings.Contains(got[1], "error") {
		t.Fatalf("status: %v", got)
	}
}

func TestExtractOpenQuestions(t *testing.T) {
	text := strings.Join([]string{
		"Done with the fix.",
		"- Should we also cover the 408 case?",
		"## Notes",
		"??",
		"What about retries on timeout?",
		"not a question",
		"And again: Should we also cover the 408 case?", // dedupe
	}, "\n")
	got := extractOpenQuestions(text, 3)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "408") {
		t.Fatalf("first: %v", got)
	}
}

func TestBriefLeft(t *testing.T) {
	e := sessionstore.Entry{}
	e.UpsertPR(sessionstore.TrackedPR{
		Number: 1, State: "OPEN", URL: "https://github.com/o/r/pull/1",
		Review: "CHANGES_REQUESTED",
	})
	left := briefLeft(BriefCardInput{Status: "idle", Turns: 1}, e, "")
	if !strings.Contains(left, "changes requested") {
		t.Fatalf("left=%q", left)
	}

	eCI := sessionstore.Entry{}
	eCI.UpsertPR(sessionstore.TrackedPR{
		Number: 2, State: "OPEN", URL: "https://github.com/o/r/pull/2",
		Checks: "✓ 3 · ✗ 1",
	})
	left = briefLeft(BriefCardInput{Status: "idle", Turns: 1}, eCI, "")
	if !strings.Contains(left, "CI failing") {
		t.Fatalf("ci left=%q", left)
	}

	left = briefLeft(BriefCardInput{Status: "running · 5s", Queue: 2, Turns: 1}, sessionstore.Entry{}, "")
	if !strings.Contains(left, "run in progress") || !strings.Contains(left, "2 follow-up") {
		t.Fatalf("left=%q", left)
	}

	left = briefLeft(BriefCardInput{Status: "idle", Turns: 0}, sessionstore.Entry{}, "")
	if !strings.Contains(left, "no work yet") {
		t.Fatalf("left=%q", left)
	}
}

func TestPreserveBriefFields(t *testing.T) {
	prev := sessionstore.Entry{Goal: "g", BriefMsgID: "mid"}
	next := sessionstore.Entry{SessionID: "s"}
	preserveBriefFields(&next, prev)
	if next.Goal != "g" || next.BriefMsgID != "mid" {
		t.Fatalf("got %+v", next)
	}
	// Do not overwrite.
	next2 := sessionstore.Entry{Goal: "new", BriefMsgID: "m2"}
	preserveBriefFields(&next2, prev)
	if next2.Goal != "new" || next2.BriefMsgID != "m2" {
		t.Fatalf("overwrite: %+v", next2)
	}
}

func TestEnsureThreadGoal(t *testing.T) {
	b := testBot(t)
	if err := b.sessions.Set("t1", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	b.ensureThreadGoal("t1", "  fix the bug now  ")
	e, _ := b.sessions.Get("t1")
	if e.Goal != "fix the bug now" {
		t.Fatalf("goal=%q", e.Goal)
	}
	b.ensureThreadGoal("t1", "other")
	e, _ = b.sessions.Get("t1")
	if e.Goal != "fix the bug now" {
		t.Fatalf("should be sticky: %q", e.Goal)
	}
}

func TestClampGoal(t *testing.T) {
	if clampGoal("  x  ") != "x" {
		t.Fatal("trim")
	}
	long := strings.Repeat("a", 500)
	got := clampGoal(long)
	if len([]rune(got)) > maxBriefGoalRunes {
		t.Fatalf("len=%d", len([]rune(got)))
	}
}
