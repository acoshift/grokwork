package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// linkStore builds an identity store with the given alias → canonical bindings.
func linkStore(t *testing.T, links map[string]string) *identity.Store {
	t.Helper()
	st, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for alias, canonical := range links {
		if err := st.Link(alias, canonical, ""); err != nil {
			t.Fatalf("link %s → %s: %v", alias, canonical, err)
		}
	}
	return st
}

// A Discord message from a linked login must mint the ACCOUNT's actor id, not
// the snowflake — otherwise the thread it starts is owned by a stranger as far
// as every web surface is concerned, and the same person is two owners.
func TestDiscordMintResolvesToCanonicalAccount(t *testing.T) {
	const (
		snowflake = "424242424242424242"
		account   = "google:sub-1"
	)
	sessions, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{sessions: sessions}
	b.SetIdentity(linkStore(t, map[string]string{snowflake: account}))

	u := &discordgo.User{ID: snowflake, Username: "someone"}
	if got := b.actorFromUser(u); got.ID != account {
		t.Fatalf("actorFromUser id=%q want %q", got.ID, account)
	}
	if got := b.userActorID(u); got != account {
		t.Fatalf("userActorID=%q want %q", got, account)
	}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{Author: u}}
	if got := b.authorActorID(m); got != account {
		t.Fatalf("authorActorID=%q want %q", got, account)
	}

	// And it reaches storage: the owner bound at the start of a run is the account.
	b.bindThreadOwner("t1", "proj", m)
	e, ok := b.sessions.Get("t1")
	if !ok {
		t.Fatal("no session written")
	}
	if e.OwnerID != account {
		t.Fatalf("OwnerID=%q want the account %q — the snowflake leaked into storage", e.OwnerID, account)
	}
}

// An unlinked user is untouched, and so is a canonical id passing through a
// second time: resolution has to be a no-op for everyone who never linked, or
// this is a migration rather than a feature.
func TestCanonicalMintIsIdentityForUnlinkedAndCanonicalIDs(t *testing.T) {
	b := &Bot{}
	b.SetIdentity(linkStore(t, map[string]string{"424242424242424242": "google:sub-1"}))

	if got := b.canonicalActorID("999888777666555444"); got != "999888777666555444" {
		t.Fatalf("unlinked snowflake was rewritten to %q", got)
	}
	if got := b.canonicalActorID("google:sub-1"); got != "google:sub-1" {
		t.Fatalf("canonical id was rewritten to %q — resolution must be idempotent", got)
	}
	if got := b.canonicalActorID(""); got != "" {
		t.Fatalf("empty id became %q", got)
	}

	// No store at all is the common case (tests, and any deployment where nobody
	// has linked): every resolver must degrade to the identity function.
	plain := &Bot{}
	if got := plain.canonicalActorID("424242424242424242"); got != "424242424242424242" {
		t.Fatalf("nil identity store rewrote %q", got)
	}
	var nilBot *Bot
	if got := nilBot.canonicalActorID("x"); got != "x" {
		t.Fatalf("nil bot rewrote %q", got)
	}
}

// This is the test that proves canonical-at-mint actually works. The per-user
// concurrency cap compares job.actorID with plain ==; an alias-aware
// config.SameActor would never be consulted here. One human arriving once
// through the web (already canonical) and once through Discord (an alias) must
// land in ONE bucket and be refused the second concurrent run.
func TestPerUserRunCapCountsOneAccountAcrossProviders(t *testing.T) {
	const (
		snowflake = "424242424242424242"
		account   = "google:sub-1"
	)
	max := 1
	b := &Bot{cfg: &config.Config{MaxConcurrentRunsUser: &max}}
	b.SetIdentity(linkStore(t, map[string]string{snowflake: account}))

	// Run 1: a web dispatch. The web session already carries the canonical id
	// (oauthCallback resolved it at login).
	web := taskItem{threadID: "t1", actor: Actor{ID: account, DisplayName: "Web Me"}}
	if claimed, _, err := b.claimOrEnqueue("t1", &runJob{cancel: func() {}, start: time.Now()}, web); err != nil || !claimed {
		t.Fatalf("web claim: claimed=%v err=%v", claimed, err)
	}

	// Run 2: the same human, on a different thread, via Discord.
	discord := taskItem{threadID: "t2", actor: b.actorFromUser(&discordgo.User{ID: snowflake, Username: "me"})}
	if discord.actor.ID != account {
		t.Fatalf("Discord mint produced %q, not the account", discord.actor.ID)
	}
	claimed, _, err := b.claimOrEnqueue("t2", &runJob{cancel: func() {}, start: time.Now()}, discord)
	if claimed || err == nil {
		t.Fatalf("two logins of one person must share the cap: claimed=%v err=%v", claimed, err)
	}
	if n := b.countActiveRunsByUser(account); n != 1 {
		t.Fatalf("active runs for the account = %d, want 1", n)
	}
	if n := b.countActiveRunsByUser(snowflake); n != 0 {
		t.Fatalf("the alias still has its own bucket (%d) — mint did not resolve", n)
	}

	// Somebody else is nowhere near the cap.
	other := taskItem{threadID: "t3", actor: Actor{ID: "999888777666555444"}}
	if claimed, _, err := b.claimOrEnqueue("t3", &runJob{cancel: func() {}, start: time.Now()}, other); err != nil || !claimed {
		t.Fatalf("unrelated actor refused: claimed=%v err=%v", claimed, err)
	}
}

func newIdentityRouteBot(t *testing.T, links map[string]string) *Bot {
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
	b := &Bot{sessions: sessions, inbox: ib}
	b.SetIdentity(linkStore(t, links))
	return b
}

// Reverse lookup. A Google-canonical account is not snowflake-shaped, so the DM
// path used to give up on it and queue an inbox entry. With a Discord login
// linked, there is a real channel to reach them on and it must be used.
func TestNotifyDMFollowsTheDiscordAlias(t *testing.T) {
	const (
		snowflake = "424242424242424242"
		account   = "google:sub-1"
	)
	b := newIdentityRouteBot(t, map[string]string{snowflake: account})
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project: "proj", Goal: "fix the flaky test", WatcherIDs: []string{account},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", account, grokrun.Result{}, 2*time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 1 || dmed[0] != snowflake {
		t.Fatalf("dmed = %v, want the linked Discord login %q", dmed, snowflake)
	}
	// Reached on Discord means no inbox copy — under either id.
	if n := b.inbox.Count(account); n != 0 {
		t.Errorf("inbox got %d for the account — the ping is doubled", n)
	}
	if n := b.inbox.Count(snowflake); n != 0 {
		t.Errorf("inbox got %d for the alias", n)
	}
}

// The fallback is unchanged for an account with no Discord login: a GitHub link
// is not a channel, and the person must still be told.
func TestNotifyDMWithoutDiscordAliasStillUsesInbox(t *testing.T) {
	const account = "google:sub-1"
	b := newIdentityRouteBot(t, map[string]string{"github:999": account})
	if err := b.sessions.Set("w_unit1", sessionstore.Entry{
		Project: "proj", WatcherIDs: []string{account},
	}); err != nil {
		t.Fatal(err)
	}

	var dmed []string
	b.notifyRunDoneDM("w_unit1", account, grokrun.Result{}, time.Second,
		func(userID, content string) error { dmed = append(dmed, userID); return nil })

	if len(dmed) != 0 {
		t.Fatalf("dmed = %v — a GitHub login is not a DM channel", dmed)
	}
	items, err := b.inbox.List(account)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("inbox items = %d, want 1 (the recipient must not be dropped)", len(items))
	}
	if items[0].UnitID != "w_unit1" {
		t.Errorf("unit = %q", items[0].UnitID)
	}
}

// The in-thread ping takes the same reverse lookup: a mention only renders from
// a snowflake, so an account with a Discord login must be pinged on it.
func TestInThreadMentionUsesTheDiscordAlias(t *testing.T) {
	const (
		snowflake = "424242424242424242"
		account   = "google:sub-1"
		thread    = "111222333444555666"
	)
	b := newIdentityRouteBot(t, map[string]string{snowflake: account})
	if err := b.sessions.Set(thread, sessionstore.Entry{
		Project: "proj", WatcherIDs: []string{account, "web:nobody"},
	}); err != nil {
		t.Fatal(err)
	}

	var gotIDs []string
	var body string
	b.notifyRunDoneSend(thread, "", grokrun.Result{}, time.Second,
		func(threadID, content string, userIDs []string) error {
			gotIDs = userIDs
			body = content
			return nil
		})

	if len(gotIDs) != 2 || gotIDs[0] != snowflake {
		t.Fatalf("recipients = %v, want the linked snowflake first", gotIDs)
	}
	if want := "<@" + snowflake + ">"; !strings.Contains(body, want) {
		t.Errorf("message %q does not mention %s", body, want)
	}
	// An account with no Discord login is left exactly as it was before links
	// existed rather than being dropped from the message.
	if gotIDs[1] != "web:nobody" {
		t.Errorf("unmappable recipient = %q, want it passed through untouched", gotIDs[1])
	}
}
