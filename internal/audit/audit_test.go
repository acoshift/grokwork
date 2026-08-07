package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendAndReadDay(t *testing.T) {
	dir := t.TempDir()
	log, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return fixed }

	if err := log.Append(Event{
		Action: ActionConfigSettings,
		Actor:  "12345",
		Role:   "admin",
		OK:     true,
		Detail: map[string]any{"section": "worktree"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{
		Action: ActionLoginFail,
		Actor:  "stranger",
		OK:     false,
		Error:  "not authorized",
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(log.Dir(), "2026-07-20.jsonl")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", st.Mode().Perm())
	}

	evs, err := log.ReadDay(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("len=%d", len(evs))
	}
	if evs[0].Action != ActionConfigSettings || evs[0].Actor != "12345" || !evs[0].OK {
		t.Fatalf("first=%+v", evs[0])
	}
	if evs[0].Time.IsZero() {
		t.Fatal("missing time")
	}
	if evs[1].Action != ActionLoginFail || evs[1].OK {
		t.Fatalf("second=%+v", evs[1])
	}
}

func TestAppendEmptyActorBecomesAnonymous(t *testing.T) {
	log, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Action: ActionWorktreePrune, OK: true}); err != nil {
		t.Fatal(err)
	}
	evs, err := log.ReadDay(time.Now())
	if err != nil || len(evs) != 1 {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
	if evs[0].Actor != ActorAnonymous {
		t.Fatalf("actor=%q", evs[0].Actor)
	}
}

func TestNilLoggerNoop(t *testing.T) {
	var log *Logger
	if err := log.Append(Event{Action: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyActionRejected(t *testing.T) {
	log, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{}); err == nil {
		t.Fatal("expected error")
	}
}

// TestAppendScrubsPathsFromEveryWriter pins the guarantee at the layer that can
// actually make it: Append. Scrubbing used to live in internal/bot, so a Discord
// /reset wrote "[path]" while the identical web action wrote gh stderr verbatim
// — the same question answered differently depending on which button the
// operator pressed. Both surfaces construct their own Logger from cfg.DataDir
// and share nothing else, so Append is the only common chokepoint.
func TestAppendScrubsPathsFromEveryWriter(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return day }

	err = l.Append(Event{
		Action: "pr.review.github",
		Actor:  "discord:1",
		// Shaped like real gh stderr, where the path is glued to a colon.
		Error: "failed to run gh: open /Users/someone/Projects/secret-client/body.md: no such file",
		Detail: map[string]any{
			"repoDir": "/srv/checkouts/secret-client",
			// gcloud quoting a storage credentials key path — /etc is in the
			// scrub allowlist because that is where key files typically live.
			"notes":  []string{"wrote /tmp/gh-body-123", "could not read /etc/grokwork/gcs-key.json", "ok"},
			"number": 7,
			"url":    "https://github.com/o/r/pull/7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	events, err := l.ReadDay(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]

	for _, leak := range []string{"/Users/someone", "/srv/checkouts", "/tmp/gh-body-123", "secret-client", "/etc/grokwork", "gcs-key.json"} {
		if strings.Contains(ev.Error, leak) {
			t.Errorf("Error leaked %q: %s", leak, ev.Error)
		}
		if strings.Contains(fmt.Sprint(ev.Detail), leak) {
			t.Errorf("Detail leaked %q: %v", leak, ev.Detail)
		}
	}

	// A URL is the most useful field in the log and must survive: a blanket
	// "redact anything with slashes" would shred every PR link.
	if got := fmt.Sprint(ev.Detail["url"]); got != "https://github.com/o/r/pull/7" {
		t.Errorf("URL was scrubbed: %q", got)
	}
	if got := fmt.Sprint(ev.Detail["number"]); got != "7" {
		t.Errorf("non-string detail mangled: %q", got)
	}
}

// TestScrubDetailDoesNotMutateCaller: call sites build detail maps inline and
// sometimes reuse them, so an audit write must not edit a caller's data.
func TestScrubDetailDoesNotMutateCaller(t *testing.T) {
	detail := map[string]any{"dir": "/Users/x/repo", "list": []string{"/home/y/z"}}
	_ = scrubDetail(detail)
	if detail["dir"] != "/Users/x/repo" {
		t.Errorf("caller's map was mutated: %v", detail["dir"])
	}
	if detail["list"].([]string)[0] != "/home/y/z" {
		t.Errorf("caller's slice was mutated: %v", detail["list"])
	}
}
