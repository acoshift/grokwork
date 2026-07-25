package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/timeline"
)

func newTimelineBot(t *testing.T) *Bot {
	t.Helper()
	st, err := timeline.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Bot{events: st}
}

// TestCancelledRunKeepsStreamedOutput is the data-loss fix. history.Turn.Response
// is result.Text with no streamer fallback, so before the timeline a cancelled
// run left a web-native unit with no record of what the agent had said — while
// the Discord path still had every sealed chunk sitting in the thread.
func TestCancelledRunKeepsStreamedOutput(t *testing.T) {
	b := newTimelineBot(t)
	b.recordRunTimeline("w_unit1", "partial work before cancel", grokrun.Result{Cancelled: true}, 3*time.Second)

	events, err := b.events.Read("w_unit1")
	if err != nil {
		t.Fatal(err)
	}
	if got := timeline.Transcript(events); got != "partial work before cancel" {
		t.Fatalf("transcript = %q, want the streamed text preserved", got)
	}

	var done timeline.RunDone
	var found bool
	for _, e := range events {
		if e.Kind == timeline.KindRunDone {
			if err := e.DecodeData(&done); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no run.done event")
	}
	if done.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", done.Status)
	}
}

func TestFinalReplyPreferredOverStreamedText(t *testing.T) {
	b := newTimelineBot(t)
	b.recordRunTimeline("123", "streamed draft", grokrun.Result{Text: "final reply"}, time.Second)
	events, _ := b.events.Read("123")
	if got := timeline.Transcript(events); got != "final reply" {
		t.Fatalf("transcript = %q, want the final reply to win", got)
	}
}

func TestRunWithNoOutputStillRecordsOutcome(t *testing.T) {
	b := newTimelineBot(t)
	b.recordRunTimeline("123", "", grokrun.Result{Code: 2}, time.Second)
	events, _ := b.events.Read("123")
	if len(events) != 1 || events[0].Kind != timeline.KindRunDone {
		t.Fatalf("events = %+v, want exactly one run.done and no empty text block", events)
	}
}

// TestRunTimelineOutcomeMatchesHistory pins the two derivations together: if they
// drift, the session page and the history view disagree about how a run ended.
func TestRunTimelineOutcomeMatchesHistory(t *testing.T) {
	cases := []struct {
		name   string
		result grokrun.Result
		status string
	}{
		{"done", grokrun.Result{}, "done"},
		{"cancelled", grokrun.Result{Cancelled: true}, "cancelled"},
		{"max turns", grokrun.Result{MaxTurnsReached: true}, "error"},
		{"nonzero exit", grokrun.Result{Code: 1}, "error"},
		{"cancelled wins over exit code", grokrun.Result{Cancelled: true, Code: 1}, "cancelled"},
	}
	for _, tc := range cases {
		got, _ := runTimelineOutcome(tc.result)
		if got != tc.status {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.status)
		}
	}
}

// TestTimelineFailureNeverBreaksARun pins invariant I3 at the write side: a nil
// store (init failed at boot) must be a silent no-op, not a panic.
func TestTimelineFailureNeverBreaksARun(t *testing.T) {
	var b *Bot // nil receiver
	b.appendTimeline("123", timeline.KindNotice, timeline.Notice{Text: "x"})
	b.recordRunTimeline("123", "text", grokrun.Result{}, time.Second)

	b2 := &Bot{} // events == nil
	b2.appendTimeline("123", timeline.KindNotice, timeline.Notice{Text: "x"})
	b2.recordRunTimeline("123", "text", grokrun.Result{}, time.Second)

	if b2.Events() != nil {
		t.Error("Events() should be nil when init failed")
	}
}

func TestInvalidUnitIDIsLoggedNotFatal(t *testing.T) {
	b := newTimelineBot(t)
	// A path-traversal id must be refused by the store and swallowed here.
	b.appendTimeline("../escape", timeline.KindNotice, timeline.Notice{Text: "x"})
	if events, err := b.events.Read("123"); err != nil || len(events) != 0 {
		t.Errorf("unexpected write: events=%+v err=%v", events, err)
	}
}

func TestOneBlockPerRunNotPerMessageCap(t *testing.T) {
	b := newTimelineBot(t)
	// Longer than the Discord 1900 cap: the timeline must not re-split it.
	long := strings.Repeat("a", 4000)
	b.recordRunTimeline("123", "", grokrun.Result{Text: long}, time.Second)
	events, _ := b.events.Read("123")
	blocks := 0
	for _, e := range events {
		if e.Kind == timeline.KindTextBlock {
			blocks++
		}
	}
	if blocks != 1 {
		t.Errorf("text blocks = %d, want 1 — Discord's message cap must not leak into the store", blocks)
	}
}

// TestCINoticeRecordedWithoutDiscord: a web-native unit had no record of CI
// notices at all — ciNotice returned before doing anything. The note is now
// recorded for every unit and only posted where there is a thread.
func TestCINoticeRecordedWithoutDiscord(t *testing.T) {
	dir := t.TempDir()
	events, err := timeline.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Set("w_ci1", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{events: events, sessions: sessions}

	// Nil session and a web-native unit: the old code was a total no-op here.
	b.ciNotice(nil, "w_ci1", "queued auto CI fix")

	evs, err := b.events.Read("w_ci1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != timeline.KindNotice {
		t.Fatalf("events = %+v, want one notice", evs)
	}
	var n timeline.Notice
	if err := evs[0].DecodeData(&n); err != nil {
		t.Fatal(err)
	}
	if n.Text != "queued auto CI fix" {
		t.Errorf("text = %q", n.Text)
	}
}

// timelineKinds returns the event kinds recorded for a unit, for coverage checks.
func timelineKinds(t *testing.T, b *Bot, unit string) []timeline.Kind {
	t.Helper()
	evs, err := b.events.Read(unit)
	if err != nil {
		t.Fatal(err)
	}
	var out []timeline.Kind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func hasKind(kinds []timeline.Kind, want timeline.Kind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func newC3Bot(t *testing.T, unit string) *Bot {
	t.Helper()
	dir := t.TempDir()
	events, err := timeline.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Set(unit, sessionstore.Entry{Project: "p", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	return &Bot{events: events, sessions: sessions}
}

// TestBriefRecordedWithoutDiscord — C3 row 2. refreshBriefCard used to fail fast
// on a nil session, so the brief content (the work summary) existed only as a
// pinned Discord message.
func TestBriefRecordedWithoutDiscord(t *testing.T) {
	b := newC3Bot(t, "w_brief")
	// Nil session: pinning fails, recording must not.
	_, err := b.refreshBriefCard(nil, "w_brief", "")
	if err == nil {
		t.Error("expected an error for the missing session (pinning cannot happen)")
	}
	if !hasKind(timelineKinds(t, b, "w_brief"), timeline.KindBrief) {
		t.Error("brief content not recorded for a unit with no Discord surface")
	}
}

// TestResumeAnnouncementRecorded — C3 row 9. announceResume posted blindly to any
// unit id, so a web-native unit got a guaranteed 4xx and no record.
func TestResumeAnnouncementRecorded(t *testing.T) {
	b := newC3Bot(t, "w_resume")
	b.announceResume("w_resume", "proj", 2)
	kinds := timelineKinds(t, b, "w_resume")
	if !hasKind(kinds, timeline.KindNotice) {
		t.Fatalf("resume not recorded, kinds = %v", kinds)
	}
	evs, _ := b.events.Read("w_resume")
	var n timeline.Notice
	if err := evs[0].DecodeData(&n); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.Text, "proj") || !strings.Contains(n.Text, "attempt 2") {
		t.Errorf("resume text = %q, want project and attempt", n.Text)
	}
}

// TestPRTimelineRecordedWithoutDiscord — C3 row 3. State was tracked in
// sessionstore, but how the PR got there was Discord-embed-only.
func TestPRTimelineRecordedWithoutDiscord(t *testing.T) {
	b := newC3Bot(t, "w_pr")
	// A review-decision transition: prev is not the first seed (it has checks), and
	// the decision changed, which is one of the cases DiffTimeline reports.
	prev := ghpr.Snapshot{State: "OPEN", Checks: "1 pending"}
	info := ghpr.Info{State: "OPEN", ReviewDecision: "APPROVED", Checks: "1 pending"}
	b.announcePRTimeline(nil, "w_pr", prev, info)
	if !hasKind(timelineKinds(t, b, "w_pr"), timeline.KindPRStatus) {
		t.Error("PR transition not recorded for a web-native unit")
	}
}
