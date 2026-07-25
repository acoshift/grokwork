package timeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

func TestAppendAssignsMonotonicSeq(t *testing.T) {
	st, _ := newStore(t)
	for i := 1; i <= 3; i++ {
		ev, err := st.Append("123", KindTextBlock, TextBlock{Text: "chunk"})
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("seq = %d, want %d", ev.Seq, i)
		}
		if ev.At == "" {
			t.Error("At not stamped")
		}
	}
}

func TestSeqSurvivesRestart(t *testing.T) {
	st, dir := newStore(t)
	if _, err := st.Append("123", KindTextBlock, TextBlock{Text: "a"}); err != nil {
		t.Fatal(err)
	}
	// New Store over the same dir: seq must continue, not restart at 1, or a
	// restart mid-run would produce duplicate seqs and break tailing.
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st2.Append("123", KindTextBlock, TextBlock{Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatalf("seq after restart = %d, want 2", ev.Seq)
	}
}

func TestReadSinceTails(t *testing.T) {
	st, _ := newStore(t)
	for _, s := range []string{"a", "b", "c"} {
		if _, err := st.Append("123", KindTextBlock, TextBlock{Text: s}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ReadSince("123", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text() != "b" || got[1].Text() != "c" {
		t.Fatalf("ReadSince(1) = %+v, want b,c", got)
	}
}

// TestTranscriptSurvivesMissingResult is the cancelled-run data loss this store
// exists to fix: blocks are committed as they seal, so output is readable even
// though no final result text was ever produced.
func TestTranscriptSurvivesMissingResult(t *testing.T) {
	st, _ := newStore(t)
	for _, s := range []string{"first part", "second part"} {
		if _, err := st.Append("w_abc", KindTextBlock, TextBlock{Text: s}); err != nil {
			t.Fatal(err)
		}
	}
	// Run is cancelled: only a run.done event, no completion, no result text.
	if _, err := st.Append("w_abc", KindRunDone, RunDone{Status: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.Read("w_abc")
	if err != nil {
		t.Fatal(err)
	}
	got := Transcript(events)
	if got != "first part\n\nsecond part" {
		t.Fatalf("Transcript = %q, want both blocks joined by a paragraph break", got)
	}
}

func TestTornLineDoesNotPoisonTimeline(t *testing.T) {
	st, dir := newStore(t)
	if _, err := st.Append("123", KindTextBlock, TextBlock{Text: "good"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: append a partial JSON line.
	f, err := os.OpenFile(filepath.Join(dir, "timeline", "123.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"kind":"text.bl`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err := st.Read("123")
	if err != nil {
		t.Fatalf("torn line made the whole timeline unreadable: %v", err)
	}
	if len(events) != 1 || events[0].Text() != "good" {
		t.Fatalf("events = %+v, want the one good event", events)
	}
}

func TestUnknownKindPreserved(t *testing.T) {
	st, dir := newStore(t)
	// A newer writer's event kind must survive an older reader.
	line := `{"seq":1,"at":"2026-01-01T00:00:00Z","kind":"future.thing","data":{"x":1}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "timeline", "555.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := st.Read("555")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "future.thing" {
		t.Fatalf("events = %+v, want the unknown kind preserved", events)
	}
	if !strings.Contains(string(events[0].Data), `"x":1`) {
		t.Errorf("payload lost: %s", events[0].Data)
	}
}

func TestOversizePayloadTruncatedNotDropped(t *testing.T) {
	st, _ := newStore(t)
	big := strings.Repeat("x", maxDataBytes+1024)
	ev, err := st.Append("123", KindTextBlock, TextBlock{Text: big})
	if err != nil {
		t.Fatalf("oversize payload must truncate, not error: %v", err)
	}
	if !strings.Contains(string(ev.Data), "truncated") {
		t.Errorf("data = %s, want a truncation marker", ev.Data)
	}
}

func TestInvalidUnitIDRejected(t *testing.T) {
	st, _ := newStore(t)
	for _, id := range []string{"", "../escape", "a/b", strings.Repeat("x", 65)} {
		if _, err := st.Append(id, KindNotice, Notice{Text: "x"}); err == nil {
			t.Errorf("Append(%q) should be rejected (path traversal)", id)
		}
		if _, err := st.ReadSince(id, 0); err == nil {
			t.Errorf("ReadSince(%q) should be rejected", id)
		}
	}
}

func TestDeleteRemovesUnit(t *testing.T) {
	st, _ := newStore(t)
	if _, err := st.Append("123", KindNotice, Notice{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("123"); err != nil {
		t.Fatal(err)
	}
	events, err := st.Read("123")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("events after delete = %d, want 0", len(events))
	}
	// Delete of a missing unit is not an error (idle sweep runs repeatedly).
	if err := st.Delete("123"); err != nil {
		t.Errorf("second delete errored: %v", err)
	}
	// Seq restarts after delete, since the file is gone.
	ev, err := st.Append("123", KindNotice, Notice{Text: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 {
		t.Errorf("seq after delete = %d, want 1", ev.Seq)
	}
}

func TestReadMissingUnitIsEmptyNotError(t *testing.T) {
	st, _ := newStore(t)
	events, err := st.Read("999")
	if err != nil {
		t.Fatalf("reading a unit with no timeline should not error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %d, want 0", len(events))
	}
}
