package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestClassifyRunOutcome(t *testing.T) {
	if classifyRunOutcome(grokrun.Result{Cancelled: true}) != outcomeCancelled {
		t.Fatal("cancel")
	}
	if classifyRunOutcome(grokrun.Result{Code: 1}) != outcomeError {
		t.Fatal("code")
	}
	if classifyRunOutcome(grokrun.Result{MaxTurnsReached: true}) != outcomeError {
		t.Fatal("max turns")
	}
	if classifyRunOutcome(grokrun.Result{Code: 0}) != outcomeOK {
		t.Fatal("ok")
	}
}

func TestShouldNotifyAuthor(t *testing.T) {
	short := 30 * time.Second
	long := 10 * time.Minute
	if shouldNotifyAuthor(config.NotifyOnDoneNever, 0, outcomeError, short) {
		t.Fatal("never")
	}
	if !shouldNotifyAuthor(config.NotifyOnDoneAlways, 0, outcomeOK, short) {
		t.Fatal("always")
	}
	if shouldNotifyAuthor(config.NotifyOnDoneErrors, 0, outcomeOK, short) {
		t.Fatal("errors+ok")
	}
	if !shouldNotifyAuthor(config.NotifyOnDoneErrors, 0, outcomeError, short) {
		t.Fatal("errors+err")
	}
	if shouldNotifyAuthor(config.NotifyOnDoneLongOnly, 60_000, outcomeOK, short) {
		t.Fatal("long short")
	}
	if !shouldNotifyAuthor(config.NotifyOnDoneLongOnly, 60_000, outcomeOK, long) {
		t.Fatal("long long")
	}
	// empty mode → errors default
	if shouldNotifyAuthor("", 0, outcomeOK, short) {
		t.Fatal("default ok")
	}
	if !shouldNotifyAuthor("", 0, outcomeCancelled, short) {
		t.Fatal("default cancel")
	}
}

func TestNotifyMentionIDs(t *testing.T) {
	// Watcher always; author only when policy says so.
	ids := notifyMentionIDs("author", []string{"w1", "w1", "w2"}, config.NotifyOnDoneNever, 0, outcomeOK, time.Second)
	if len(ids) != 2 || ids[0] != "w1" || ids[1] != "w2" {
		t.Fatalf("watchers only: %v", ids)
	}
	ids = notifyMentionIDs("author", []string{"w1"}, config.NotifyOnDoneAlways, 0, outcomeOK, time.Second)
	if len(ids) != 2 {
		t.Fatalf("author+watcher: %v", ids)
	}
	// Author already watcher → one id
	ids = notifyMentionIDs("w1", []string{"w1"}, config.NotifyOnDoneAlways, 0, outcomeOK, time.Second)
	if len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("dedupe: %v", ids)
	}
	// Errors policy + success → watchers only
	ids = notifyMentionIDs("author", []string{"w1"}, config.NotifyOnDoneErrors, 0, outcomeOK, time.Second)
	if len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("errors success: %v", ids)
	}
}

func TestFormatNotifyDoneMessage(t *testing.T) {
	msg := formatNotifyDoneMessage([]string{"1", "2"}, outcomeError, 90*time.Second)
	if !strings.Contains(msg, "<@1>") || !strings.Contains(msg, "<@2>") {
		t.Fatalf("mentions: %s", msg)
	}
	if !strings.Contains(msg, "**failed**") {
		t.Fatalf("label: %s", msg)
	}
	if formatNotifyDoneMessage(nil, outcomeOK, 0) != "" {
		t.Fatal("empty")
	}
}

func TestWatcherAddRemove(t *testing.T) {
	var e sessionstore.Entry
	if !e.AddWatcher("u1") {
		t.Fatal("add")
	}
	if e.AddWatcher("u1") {
		t.Fatal("dedupe")
	}
	if !e.IsWatcher("u1") || len(e.WatcherIDs) != 1 {
		t.Fatal(e.WatcherIDs)
	}
	if !e.RemoveWatcher("u1") {
		t.Fatal("remove")
	}
	if e.IsWatcher("u1") || e.RemoveWatcher("u1") {
		t.Fatal("gone")
	}
}

func TestPreserveWatcherFields(t *testing.T) {
	prev := sessionstore.Entry{WatcherIDs: []string{"a", "b"}}
	next := sessionstore.Entry{}
	preserveWatcherFields(&next, prev)
	if len(next.WatcherIDs) != 2 {
		t.Fatalf("%v", next.WatcherIDs)
	}
	// non-empty next wins
	next2 := sessionstore.Entry{WatcherIDs: []string{"c"}}
	preserveWatcherFields(&next2, prev)
	if len(next2.WatcherIDs) != 1 || next2.WatcherIDs[0] != "c" {
		t.Fatalf("%v", next2.WatcherIDs)
	}
}

func TestParseWatchCommands(t *testing.T) {
	if ParseMessage("<@BOT> /watch", "BOT").Kind != KindWatch {
		t.Fatal("watch")
	}
	if ParseMessage("<@BOT> /unwatch", "BOT").Kind != KindUnwatch {
		t.Fatal("unwatch")
	}
	if ParseMessage("<@BOT> watch the logs carefully", "BOT").Kind != KindTask {
		t.Fatal("freeform watch must stay task")
	}
}

func TestHelpMentionsWatch(t *testing.T) {
	h := HelpText()
	if !strings.Contains(h, "/watch") || !strings.Contains(h, "/unwatch") {
		t.Fatal(h)
	}
}
