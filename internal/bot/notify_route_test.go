package bot

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

var errNotifyBoom = errors.New("discord down")

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
		Project: "proj", Goal: "fix the flaky test", WatcherIDs: []string{"oidc:alice"},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", "oidc:alice", grokrun.Result{}, 2*time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 0 {
		t.Errorf("DMed %v — a non-Discord id has no DM channel", dmed)
	}
	items, err := b.inbox.List("oidc:alice")
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
	if items[0].URL != "/sessions/w_unit1?project=proj" {
		t.Errorf("url = %q, want a root-relative session path", items[0].URL)
	}
}

// TestDiscordRecipientStillDMed: Discord DM is extra, not instead of the inbox.
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
	if n := b.inbox.Count("123456789012345678"); n != 1 {
		t.Errorf("inbox count = %d, want 1 (DM does not replace the feed)", n)
	}
}

// TestMixedRecipientsSplitByReachability: each recipient takes the best channel
// that can reach them, and nobody is lost.
func TestMixedRecipientsSplitByReachability(t *testing.T) {
	b := newRouteBot(t)
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project:    "proj",
		WatcherIDs: []string{"123456789012345678", "oidc:alice", "web:bob"},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", "", grokrun.Result{}, time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 1 || dmed[0] != "123456789012345678" {
		t.Errorf("dmed = %v, want only the Discord id", dmed)
	}
	for _, id := range []string{"123456789012345678", "oidc:alice", "web:bob"} {
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
	for _, id := range gotIDs {
		if n := b.inbox.Count(id); n != 1 {
			t.Errorf("inbox count for %s = %d, want 1 on the account (and still one Discord send)", id, n)
		}
	}
}

func TestInboxWrittenBeforeFailedDiscordSend(t *testing.T) {
	b := newRouteBot(t)
	const thread = "111222333444555666"
	if err := b.sessions.Set(thread, sessionstore.Entry{
		Project: "proj", WatcherIDs: []string{"123456789012345678"},
	}); err != nil {
		t.Fatal(err)
	}
	b.notifyRunDoneSend(thread, "", grokrun.Result{}, time.Second,
		func(string, string, []string) error { return errNotifyBoom })
	if n := b.inbox.Count("123456789012345678"); n != 1 {
		t.Fatalf("inbox after failed send = %d, want 1", n)
	}
}

func TestWebWatcherGetsRunDoneThenUnwatchStops(t *testing.T) {
	b := newRouteBot(t)
	const unit = "w_watch"
	if err := b.sessions.Set(unit, sessionstore.Entry{
		Project: "proj", WatcherIDs: []string{"oidc:alice"},
	}); err != nil {
		t.Fatal(err)
	}
	b.notifyRunDoneDM(unit, "someone-else", grokrun.Result{}, time.Second,
		func(string, string) error { return nil })
	if n := b.inbox.Count("oidc:alice"); n != 1 {
		t.Fatalf("watcher inbox after run = %d", n)
	}
	if _, ok, err := b.sessions.Patch(unit, func(e *sessionstore.Entry) {
		e.RemoveWatcher("oidc:alice")
	}); err != nil || !ok {
		t.Fatalf("unwatch: ok=%v err=%v", ok, err)
	}
	b.notifyRunDoneDM(unit, "someone-else", grokrun.Result{}, time.Second,
		func(string, string) error { return nil })
	if n := b.inbox.Count("oidc:alice"); n != 1 {
		t.Fatalf("unwatched watcher got another item: %d", n)
	}
}

func TestDMCapStillInboxesEveryone(t *testing.T) {
	b := newRouteBot(t)
	watchers := make([]string, maxNotifyDMs+2)
	for i := range watchers {
		watchers[i] = "1234567890123456" + strconv.Itoa(10+i)
	}
	if err := b.sessions.Set("w_cap", sessionstore.Entry{
		Project: "proj", WatcherIDs: watchers,
	}); err != nil {
		t.Fatal(err)
	}
	var dmed int
	b.notifyRunDoneDM("w_cap", "", grokrun.Result{}, time.Second,
		func(string, string) error { dmed++; return nil })
	if dmed != maxNotifyDMs {
		t.Fatalf("DMs = %d, want cap %d", dmed, maxNotifyDMs)
	}
	for _, id := range watchers {
		if n := b.inbox.Count(id); n != 1 {
			t.Fatalf("inbox for %s = %d, cap must not drop the feed", id, n)
		}
	}
}

func TestInboxSessionPathIsRootRelative(t *testing.T) {
	if got := inboxSessionPath("w_1", "proj"); got != "/sessions/w_1?project=proj" {
		t.Fatalf("got %q", got)
	}
	if got := inboxSessionPath("abc", ""); got != "/sessions/abc" {
		t.Fatalf("no project: %q", got)
	}
	if inboxSessionPath("", "proj") != "" {
		t.Fatal("empty unit")
	}
}

func TestInboxFailureNeverPanics(t *testing.T) {
	var b *Bot
	b.deliverInbox([]string{"oidc:alice"}, "w_1", outcomeOK, time.Second)

	b2 := &Bot{} // inbox == nil
	b2.deliverInbox([]string{"oidc:alice"}, "w_1", outcomeOK, time.Second)
	if b2.Inbox() != nil {
		t.Error("Inbox() should be nil when init failed")
	}
	if err := b2.QueueInbox("oidc:alice", "k", "s", "", "", "", ""); err != nil {
		t.Errorf("QueueInbox with no store should be a no-op, got %v", err)
	}
}
