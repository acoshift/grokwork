package bot

import (
	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/identity"
)

// Canonical-at-mint, Discord side.
//
// In this package an actor id is only ever BORN from a *discordgo.User: the
// author of a message, the clicker of a button, the target of a hand-off. Every
// one of those births goes through actorFromUser / userActorID / authorActorID
// below, which resolve the snowflake to the account it belongs to before it can
// be stored, compared or charged against a cap.
//
// That is the whole mechanism. Nothing downstream is alias-aware: ownership
// still compares with ==, grants still go through config.SameActor, the
// concurrency cap still buckets on a map key. They keep working because an
// alias never reaches them — see the package comment on internal/identity for
// why teaching those comparisons about aliases instead would have been worse.
//
// Resolution is idempotent (a canonical id is never itself an alias — the store
// refuses chains from both ends), so an actor id that arrived from the web,
// already canonical, is unaffected by passing through here again.

// SetIdentity installs the identity-link store. main.go builds it and hands the
// SAME instance to the bot and the web server: two Stores over one file would
// each cache their own map and silently diverge on the first link.
//
// Set once at startup, before the gateway opens. A nil store leaves every
// resolver below an identity function, which is exactly what a deployment
// (or a test) with no links wants.
func (b *Bot) SetIdentity(store *identity.Store) {
	if b == nil {
		return
	}
	b.identity = store
}

// Identity returns the identity-link store (nil when none was installed).
func (b *Bot) Identity() *identity.Store {
	if b == nil {
		return nil
	}
	return b.identity
}

// canonicalActorID resolves a freshly minted actor id to its account.
func (b *Bot) canonicalActorID(id string) string {
	if b == nil {
		return id
	}
	return b.identity.Canonical(id)
}

// actorFromUser is the Discord mint funnel: ActorFromUser plus canonical
// resolution. The display name stays whatever Discord says — the account is
// shared, the profile that produced this particular message is not.
func (b *Bot) actorFromUser(u *discordgo.User) Actor {
	a := ActorFromUser(u)
	a.ID = b.canonicalActorID(a.ID)
	return a
}

// userActorID is actorFromUser when only the id is wanted (button clicks,
// mentioned hand-off targets). Nil-safe: an unresolvable user is "".
func (b *Bot) userActorID(u *discordgo.User) string {
	if u == nil {
		return ""
	}
	return b.canonicalActorID(u.ID)
}

// authorActorID is userActorID for a message's author. Nil-safe at both hops,
// so call sites keep the shape they had when they read m.Author.ID behind a
// nil check.
func (b *Bot) authorActorID(m *discordgo.MessageCreate) string {
	if m == nil {
		return ""
	}
	return b.userActorID(m.Author)
}

// discordDMTarget resolves an account back to a Discord snowflake that can be
// mentioned or DMed.
//
// This is the one place the arrow runs backwards, and it exists because
// canonical-at-mint deliberately allows an account whose canonical id is NOT a
// snowflake: someone who signed into the web with Google first and linked their
// Discord login afterwards is "google:sub" everywhere, including in WatcherIDs
// and as a run's author. Discord cannot deliver to that string. Their Discord
// alias can.
//
// ok is false when no Discord login is linked, and callers must respect it
// rather than trying the id anyway — posting a namespaced id to
// UserChannelCreate is a guaranteed 4xx, and the inbox is the fallback that
// actually reaches the person.
func (b *Bot) discordDMTarget(actorID string) (string, bool) {
	if looksLikeDiscordUserID(actorID) {
		return actorID, true
	}
	if b == nil {
		return "", false
	}
	for _, alias := range b.identity.AliasesOf(actorID) {
		// AliasesOf hands back wire form, so a Discord alias is already bare.
		if looksLikeDiscordUserID(alias) {
			return alias, true
		}
	}
	return "", false
}

// discordMentionIDs maps a recipient list to what <@…> can address.
//
// Unlike discordDMTarget's caller this does NOT drop the unresolvable: the
// in-thread message is one message to everybody, and an account with no Discord
// login still belongs in the line that says the run finished. Discord renders
// the un-mappable id as literal text, which is what it did before links existed.
func (b *Bot) discordMentionIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if mention, ok := b.discordDMTarget(id); ok {
			out = append(out, mention)
			continue
		}
		out = append(out, id)
	}
	return out
}
