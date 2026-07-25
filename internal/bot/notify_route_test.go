package bot

import (
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func newRouteBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	sessions, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Bot{sessions: sessions, inbox: ib}
}

// TestUnreachableRecipientGoesToInbox is the C4 fix. A web-only member's actor id
// is not a Discord snowflake, so it was removed from the recipient list and they
// were simply never told their run finished. "Cannot push to them" must mean
// "queued for them", not "dropped".
func TestUnreachableRecipientGoesToInbox(t *testing.T) {
	b := newRouteBot(t)
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project: "proj", Goal: "fix the flaky test", WatcherIDs: []string{"local:alice"},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", "local:alice", grokrun.Result{}, 2*time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 0 {
		t.Errorf("DMed %v — a non-Discord id has no DM channel", dmed)
	}
	items, err := b.inbox.List("local:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("inbox items = %d, want 1 (the recipient must not be dropped)", len(items))
	}
	if items[0].Kind != "run.done" {
		t.Errorf("kind = %q", items[0].Kind)
	}
	if items[0].Project != "proj" {
		t.Errorf("project = %q, want proj — an inbox entry has no ambient context", items[0].Project)
	}
	if items[0].UnitID != "w_unit1" {
		t.Errorf("unit = %q", items[0].UnitID)
	}
}

// TestDiscordRecipientStillDMed keeps the existing path intact: a Discord actor
// gets a DM and must NOT also get an inbox copy, or every ping is doubled.
func TestDiscordRecipientStillDMed(t *testing.T) {
	b := newRouteBot(t)
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project: "proj", WatcherIDs: []string{"123456789012345678"},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", "123456789012345678", grokrun.Result{}, time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 1 || dmed[0] != "123456789012345678" {
		t.Fatalf("dmed = %v, want the one Discord recipient", dmed)
	}
	if n := b.inbox.Count("123456789012345678"); n != 0 {
		t.Errorf("inbox got %d items for a DMed recipient — notification doubled", n)
	}
}

// TestMixedRecipientsSplitByReachability: each recipient takes the best channel
// that can reach them, and nobody is lost.
func TestMixedRecipientsSplitByReachability(t *testing.T) {
	b := newRouteBot(t)
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project:    "proj",
		WatcherIDs: []string{"123456789012345678", "local:alice", "web:bob"},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", "", grokrun.Result{}, time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 1 || dmed[0] != "123456789012345678" {
		t.Errorf("dmed = %v, want only the Discord id", dmed)
	}
	for _, id := range []string{"local:alice", "web:bob"} {
		if n := b.inbox.Count(id); n != 1 {
			t.Errorf("inbox count for %s = %d, want 1", id, n)
		}
	}
}

// TestInThreadPingStaysASingleMessage pins invariant I1 at the delivery level: a
// thread unit produces ONE send carrying every recipient, never one per person.
// Asserting on the count is the point — a content-only assertion passes even when
// delivery has fanned out into N messages.
func TestInThreadPingStaysASingleMessage(t *testing.T) {
	b := newRouteBot(t)
	if err := b.sessions.Set("111222333444555666", sessionstore.Entry{
		Project:    "proj",
		WatcherIDs: []string{"123456789012345678", "223456789012345678"},
	}); err != nil {
		t.Fatal(err)
	}

	sends := 0
	var gotIDs []string
	b.notifyRunDoneSend("111222333444555666", "323456789012345678", grokrun.Result{}, time.Second,
		func(threadID, content string, userIDs []string) error {
			sends++
			gotIDs = userIDs
			return nil
		})

	if sends != 1 {
		t.Fatalf("sends = %d, want exactly 1 in-thread message for the whole set", sends)
	}
	// Both watchers ride the one message. The author is absent by policy, not by
	// accident: notifyOnDone defaults to not pinging the author of a short
	// successful run, so only the opted-in watchers are recipients here.
	if len(gotIDs) != 2 {
		t.Errorf("recipients on the single message = %v, want both watchers", gotIDs)
	}
	// And no inbox copies: they were reached in the thread.
	for _, id := range gotIDs {
		if n := b.inbox.Count(id); n != 0 {
			t.Errorf("inbox got %d for %s — thread recipients must not be doubled", n, id)
		}
	}
}

func TestInboxFailureNeverPanics(t *testing.T) {
	var b *Bot
	b.deliverInbox([]string{"local:alice"}, "w_1", outcomeOK, time.Second)

	b2 := &Bot{} // inbox == nil
	b2.deliverInbox([]string{"local:alice"}, "w_1", outcomeOK, time.Second)
	if b2.Inbox() != nil {
		t.Error("Inbox() should be nil when init failed")
	}
	if err := b2.QueueInbox("local:alice", "k", "s", "", "", "", ""); err != nil {
		t.Errorf("QueueInbox with no store should be a no-op, got %v", err)
	}
}
