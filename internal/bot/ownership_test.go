package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func TestCanControlThreadSoftUnowned(t *testing.T) {
	b := testBot(t)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author:    &discordgo.User{ID: "u1", Username: "alice"},
		ChannelID: "t1",
	}}
	// No session owner → anyone may control (soft open / legacy).
	if !b.canControlThread(nil, m, sessionstore.Entry{}) {
		t.Fatal("unowned should allow control")
	}
}

func TestCanControlThreadOwnerAndCoOwner(t *testing.T) {
	b := testBot(t)
	e := sessionstore.Entry{OwnerID: "owner", OwnerName: "O"}
	e.AddCoOwner("co")

	ownerMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "owner"}, ChannelID: "t1",
	}}
	coMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "co"}, ChannelID: "t1",
	}}
	otherMsg := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "other"}, ChannelID: "t1",
	}}

	if !b.canControlThread(nil, ownerMsg, e) {
		t.Fatal("owner should control")
	}
	if !b.canControlThread(nil, coMsg, e) {
		t.Fatal("co-owner should control")
	}
	// Without a Session, isModerator is false → other denied.
	if b.canControlThread(nil, otherMsg, e) {
		t.Fatal("other should not control without mod perms")
	}
}

func TestEnsureSessionOwner(t *testing.T) {
	var e sessionstore.Entry
	ensureSessionOwner(&e, "u1", "alice")
	if e.OwnerID != "u1" || e.OwnerName != "alice" {
		t.Fatalf("got %+v", e)
	}
	ensureSessionOwner(&e, "u2", "bob")
	if e.OwnerID != "u1" {
		t.Fatalf("should not overwrite: %+v", e)
	}
}

func TestBindThreadOwner(t *testing.T) {
	b := testBot(t)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "u1", Username: "alice"},
	}}
	b.bindThreadOwner("t1", "app", m)
	e, ok := b.sessions.Get("t1")
	if !ok || e.OwnerID != "u1" || e.Project != "app" {
		t.Fatalf("bind new: ok=%v e=%+v", ok, e)
	}
	// Second binder does not steal ownership; keeps project.
	m2 := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author: &discordgo.User{ID: "u2", Username: "bob"},
	}}
	b.bindThreadOwner("t1", "other", m2)
	e, _ = b.sessions.Get("t1")
	if e.OwnerID != "u1" || e.Project != "app" {
		t.Fatalf("bind should be no-op when owned: %+v", e)
	}

	// Existing session without owner gets owner, preserves session id.
	if err := b.sessions.Set("t2", sessionstore.Entry{SessionID: "sid", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	b.bindThreadOwner("t2", "api", m)
	e, _ = b.sessions.Get("t2")
	if e.OwnerID != "u1" || e.SessionID != "sid" {
		t.Fatalf("bind existing: %+v", e)
	}
}

func TestPreserveOwnershipFields(t *testing.T) {
	prev := sessionstore.Entry{OwnerID: "o1", OwnerName: "A", CoOwnerIDs: []string{"c1"}}
	next := sessionstore.Entry{SessionID: "s"}
	preserveOwnershipFields(&next, prev)
	if next.OwnerID != "o1" || next.OwnerName != "A" || len(next.CoOwnerIDs) != 1 || next.CoOwnerIDs[0] != "c1" {
		t.Fatalf("got %+v", next)
	}
	// Do not clobber explicit next owner.
	next2 := sessionstore.Entry{OwnerID: "o2", OwnerName: "B"}
	preserveOwnershipFields(&next2, prev)
	if next2.OwnerID != "o2" {
		t.Fatalf("clobbered owner: %+v", next2)
	}
}

func TestPreservePRFieldsKeepsOwnership(t *testing.T) {
	prev := sessionstore.Entry{
		OwnerID: "o1", OwnerName: "A", CoOwnerIDs: []string{"c1"},
		PRNumber: 9, PRURL: "https://github.com/o/r/pull/9",
	}
	next := sessionstore.Entry{SessionID: "s", Project: "p"}
	preservePRFields(&next, prev)
	if next.OwnerID != "o1" || next.PRNumber != 9 {
		t.Fatalf("got %+v", next)
	}
}

func TestFirstMentionedUserSkipsBot(t *testing.T) {
	s := &discordgo.Session{State: &discordgo.State{}}
	s.State.User = &discordgo.User{ID: "bot"}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Mentions: []*discordgo.User{
			{ID: "bot", Username: "Grok"},
			{ID: "u9", Username: "bob"},
		},
	}}
	u := firstMentionedUser(s, m)
	if u == nil || u.ID != "u9" {
		t.Fatalf("got %+v", u)
	}
}

func TestFormatHandOffCard(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("t1", history.Turn{
		User: "alice", Prompt: "fix the flaky payment timeout in checkout", Status: "done", Project: "app",
	}); err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, hist)
	e := sessionstore.Entry{
		Project: "app", WorktreeBranch: "grok/discord/t1",
		OwnerID: "u2", OwnerName: "bob",
	}
	from := &discordgo.User{ID: "u1", Username: "alice"}
	to := &discordgo.User{ID: "u2", Username: "bob"}
	card := b.formatHandOffCard("t1", e, from, to)
	for _, want := range []string{"Hand-off", "app", "fix the flaky", "grok/discord/t1", "owns cancel/reset"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q:\n%s", want, card)
		}
	}
}

func TestLastPromptPreview(t *testing.T) {
	dir := t.TempDir()
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, hist)
	if got := b.lastPromptPreview("missing"); got != "" {
		t.Fatalf("empty: %q", got)
	}
	long := strings.Repeat("word ", 50)
	if err := hist.Append("t1", history.Turn{Prompt: long, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	got := b.lastPromptPreview("t1")
	if got == "" || len(got) > 170 {
		t.Fatalf("preview=%q len=%d", got, len(got))
	}
}

func testBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(&config.Config{DataDir: dir}, store, hist)
}
