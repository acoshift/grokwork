package bot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
		t.Fatal("long short ok")
	}
	if !shouldNotifyAuthor(config.NotifyOnDoneLongOnly, 60_000, outcomeOK, long) {
		t.Fatal("long long ok")
	}
	// long_only still pings on short failures
	if !shouldNotifyAuthor(config.NotifyOnDoneLongOnly, 60_000, outcomeError, short) {
		t.Fatal("long short err")
	}
	if !shouldNotifyAuthor(config.NotifyOnDoneLongOnly, 60_000, outcomeCancelled, short) {
		t.Fatal("long short cancel")
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
	ids := notifyMentionIDs("author", []string{"w1", "w1", "w2"}, config.NotifyOnDoneNever, 0, outcomeOK, time.Second)
	if len(ids) != 2 || ids[0] != "w1" || ids[1] != "w2" {
		t.Fatalf("watchers only: %v", ids)
	}
	ids = notifyMentionIDs("author", []string{"w1"}, config.NotifyOnDoneAlways, 0, outcomeOK, time.Second)
	if len(ids) != 2 {
		t.Fatalf("author+watcher: %v", ids)
	}
	ids = notifyMentionIDs("w1", []string{"w1"}, config.NotifyOnDoneAlways, 0, outcomeOK, time.Second)
	if len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("dedupe: %v", ids)
	}
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

func TestWatcherCap(t *testing.T) {
	var e sessionstore.Entry
	for i := 0; i < sessionstore.MaxWatchers; i++ {
		if !e.AddWatcher("w" + strconv.Itoa(i)) {
			t.Fatalf("add %d", i)
		}
	}
	if e.AddWatcher("overflow") {
		t.Fatal("cap should reject")
	}
	if len(e.WatcherIDs) != sessionstore.MaxWatchers {
		t.Fatalf("len=%d", len(e.WatcherIDs))
	}
}

func TestPreserveWatcherFields(t *testing.T) {
	prev := sessionstore.Entry{WatcherIDs: []string{"a", "b"}}
	next := sessionstore.Entry{}
	preserveWatcherFields(&next, prev)
	if len(next.WatcherIDs) != 2 {
		t.Fatalf("%v", next.WatcherIDs)
	}
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
	if strings.Contains(h, "once") {
		t.Fatalf("help must not promise one-shot: %s", h)
	}
	if !strings.Contains(h, "until") {
		t.Fatalf("help should say until /unwatch: %s", h)
	}
}

// TestNotifyRunDoneSend drives the real notifyRunDoneSend path with session
// store + config and an injectable sender (no Discord network).
func TestNotifyRunDoneSend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["author","w1"]}},
  "channels": {},
  "grokBin": "grok",
  "notifyOnDone": "errors"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	cfg.DataDir = dir

	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "123456789012345678" // Discord snowflake form, not web unit
	if err := store.Set(threadID, sessionstore.Entry{
		Project:    "app",
		WatcherIDs: []string{"w1", "w2"},
	}); err != nil {
		t.Fatal(err)
	}

	b := &Bot{cfg: cfg, sessions: store}

	var gotThread, gotContent string
	var gotUsers []string
	calls := 0
	send := func(tid, content string, users []string) error {
		calls++
		gotThread, gotContent = tid, content
		gotUsers = append([]string(nil), users...)
		return nil
	}

	// Success + errors policy → watchers only (no author)
	b.notifyRunDoneSend(threadID, "author", grokrun.Result{Code: 0}, time.Minute, send)
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if gotThread != threadID {
		t.Fatalf("thread=%q", gotThread)
	}
	if !strings.Contains(gotContent, "<@w1>") || !strings.Contains(gotContent, "<@w2>") {
		t.Fatalf("content=%s", gotContent)
	}
	if strings.Contains(gotContent, "<@author>") {
		t.Fatalf("author should not be pinged on success under errors: %s", gotContent)
	}
	if len(gotUsers) != 2 {
		t.Fatalf("users=%v", gotUsers)
	}

	// Failure → author + watchers
	calls = 0
	gotUsers = nil
	b.notifyRunDoneSend(threadID, "author", grokrun.Result{Code: 1}, 5*time.Second, send)
	if calls != 1 {
		t.Fatalf("fail calls=%d", calls)
	}
	if !strings.Contains(gotContent, "<@author>") || !strings.Contains(gotContent, "**failed**") {
		t.Fatalf("fail content=%s", gotContent)
	}
	if len(gotUsers) != 3 {
		t.Fatalf("fail users=%v", gotUsers)
	}

	// Web unit → never posted as if it were a channel (it is DMed instead; see
	// TestNotifyRunDoneDM).
	calls = 0
	b.notifyRunDoneSend("w_abc123def", "author", grokrun.Result{Code: 1}, time.Second, send)
	if calls != 0 {
		t.Fatal("web unit id must never be used as a Discord channel")
	}

	// Pre-run fail helper
	calls = 0
	b.notifyRunDoneSend(threadID, "author", grokrun.Result{Code: 1}, time.Second, send)
	if calls != 1 || !strings.Contains(gotContent, "**failed**") {
		t.Fatalf("synthetic fail: calls=%d content=%s", calls, gotContent)
	}
}

// A web-native unit has no channel, so the finished-run ping is DMed to each
// recipient instead — one DM each, since a shared "@you @them" message makes no
// sense in a private conversation.
func TestNotifyRunDoneDM(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:          dir,
		NotifyOnDone:     config.NotifyOnDoneAlways,
		WebPublicBaseURL: "https://grok.example/",
	}
	const unit = "w_abc123def456"
	if err := store.Set(unit, sessionstore.Entry{
		Project: "app",
		Goal:    "Review abcdef0: fix nil deref",
		// A non-snowflake watcher (a web login that was not Discord OAuth) cannot be
		// DMed and must be dropped before the API rejects it.
		WatcherIDs: []string{"222222222222222222", "web:local-admin"},
	}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: cfg, sessions: store}

	sent := map[string]string{}
	dm := func(userID, content string) error {
		sent[userID] = content
		return nil
	}
	b.notifyRunDoneDM(unit, "111111111111111111", grokrun.Result{Code: 0}, 25*time.Minute, dm)

	if len(sent) != 2 {
		t.Fatalf("want one DM per DM-able recipient, got %d: %v", len(sent), sent)
	}
	for _, id := range []string{"111111111111111111", "222222222222222222"} {
		body, ok := sent[id]
		if !ok {
			t.Fatalf("no DM for %s", id)
		}
		// A DM carries no ambient context, so it must name the work and link to it.
		for _, want := range []string{"**done**", "app", "Review abcdef0", "https://grok.example/sessions/" + unit} {
			if !strings.Contains(body, want) {
				t.Fatalf("DM to %s missing %q:\n%s", id, want, body)
			}
		}
		// Never address a DM with mentions of the other recipients.
		if strings.Contains(body, "<@") {
			t.Fatalf("DM must not @mention: %s", body)
		}
	}
	if _, leaked := sent["web:local-admin"]; leaked {
		t.Fatal("non-snowflake id must not be DMed")
	}

	// Policy still applies: never = nobody, even on failure.
	cfg.NotifyOnDone = config.NotifyOnDoneNever
	if err := store.Set(unit, sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	sent = map[string]string{}
	b.notifyRunDoneDM(unit, "111111111111111111", grokrun.Result{Code: 1}, time.Minute, dm)
	if len(sent) != 0 {
		t.Fatalf("notifyOnDone=never must not DM: %v", sent)
	}
}

// One recipient with DMs closed must not silence the others.
func TestNotifyRunDoneDMContinuesPastFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const unit = "w_ff0011223344"
	if err := store.Set(unit, sessionstore.Entry{
		Project:    "app",
		WatcherIDs: []string{"222222222222222222", "333333333333333333"},
	}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: &config.Config{DataDir: dir, NotifyOnDone: config.NotifyOnDoneAlways}, sessions: store}

	var attempted []string
	dm := func(userID, content string) error {
		attempted = append(attempted, userID)
		if userID == "222222222222222222" {
			return errors.New("Cannot send messages to this user")
		}
		return nil
	}
	b.notifyRunDoneDM(unit, "", grokrun.Result{Code: 0}, time.Minute, dm)
	if len(attempted) != 2 {
		t.Fatalf("a blocked recipient stopped the loop: %v", attempted)
	}
}

// Without webPublicBaseURL there is no link, but the DM must still identify the unit
// rather than saying only "run done".
func TestNotifyRunDoneDMWithoutBaseURL(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const unit = "w_9988776655"
	if err := store.Set(unit, sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: &config.Config{DataDir: dir, NotifyOnDone: config.NotifyOnDoneAlways}, sessions: store}
	var body string
	b.notifyRunDoneDM(unit, "111111111111111111", grokrun.Result{Code: 1}, time.Minute,
		func(_, content string) error { body = content; return nil })
	if !strings.Contains(body, unit) || !strings.Contains(body, "**failed**") {
		t.Fatalf("body=%q", body)
	}
}

func TestLooksLikeDiscordUserID(t *testing.T) {
	for _, ok := range []string{"111111111111111111", "12345678901234567", "12345678901234567890"} {
		if !looksLikeDiscordUserID(ok) {
			t.Fatalf("%q should be a snowflake", ok)
		}
	}
	for _, bad := range []string{"", "web:x", "1234567890123456", "123456789012345678901", "11111111111111111a"} {
		if looksLikeDiscordUserID(bad) {
			t.Fatalf("%q should not be a snowflake", bad)
		}
	}
}
