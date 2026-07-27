package config

import "strings"

// Actor id namespaces.
//
// Every id in this config used to be a Discord snowflake, which is why a
// non-Discord person could not be named in an allowlist at all — there was no
// way to say "this id is not a snowflake". Ids may now carry a namespace, and a
// bare id keeps meaning Discord so no existing config has to change.
//
// ActorKindWeb is not new: sessionstore.Entry.CreatedBy has recorded "web:<id>"
// for web-created units since that feature shipped, and this makes the same
// spelling valid in an allowlist.
// ActorKindGoogle and ActorKindGitHub are the login providers added alongside
// Discord. They get one namespace each, deliberately NOT a shared "oidc:" one:
// subject spaces are independent per issuer, so a GitHub user id of 12345 and a
// Google "sub" of 12345 are different people. Folding them together would let
// either inherit the other's team memberships and admin rights.
const (
	ActorKindDiscord = "discord"
	ActorKindWeb     = "web"
	ActorKindOIDC    = "oidc"
	ActorKindGoogle  = "google"
	ActorKindGitHub  = "github"
)

// knownActorKinds are the namespaces this build understands.
var knownActorKinds = map[string]struct{}{
	ActorKindDiscord: {},
	ActorKindWeb:     {},
	ActorKindOIDC:    {},
	ActorKindGoogle:  {},
	ActorKindGitHub:  {},
}

// NormalizeActorID canonicalizes an actor id to "<kind>:<subject>".
//
//	"1234567890"      → "discord:1234567890"   (bare == legacy snowflake)
//	"discord:1234"    → "discord:1234"
//	"OIDC:Alice"      → "oidc:Alice"           (kind case-folded, subject is not)
//	"weird:thing"     → "weird:thing"          (unknown kind passed through)
//
// An unknown namespace is deliberately passed through rather than coerced to
// Discord: guessing would silently grant a stranger whatever the id resembled.
// It simply matches nothing but itself.
func NormalizeActorID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	kind, subject, found := strings.Cut(id, ":")
	if !found {
		return ActorKindDiscord + ":" + id
	}
	lower := strings.ToLower(strings.TrimSpace(kind))
	if _, ok := knownActorKinds[lower]; !ok {
		return id
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ""
	}
	return lower + ":" + subject
}

// SameActor reports whether two ids denote the same actor, so a config written
// as "discord:123" matches a runtime id of "123" and vice versa.
func SameActor(a, b string) bool {
	na, nb := NormalizeActorID(a), NormalizeActorID(b)
	return na != "" && na == nb
}

// ActorKind returns the namespace of an id ("discord" for a bare one).
func ActorKind(id string) string {
	n := NormalizeActorID(id)
	if n == "" {
		return ""
	}
	kind, _, _ := strings.Cut(n, ":")
	return kind
}

// ActorSubject returns the subject half of an actor id ("discord:123" → "123").
// The web UI resolves display names by raw Discord snowflake, so a namespaced id
// has to be reduced before it can be looked up.
func ActorSubject(id string) string {
	n := NormalizeActorID(id)
	if n == "" {
		return ""
	}
	_, subject, found := strings.Cut(n, ":")
	if !found {
		return n
	}
	return subject
}

// IsDiscordActor reports whether an id denotes a Discord user — the only kind
// that can be sent a DM or looked up for guild roles.
func IsDiscordActor(id string) bool {
	return ActorKind(id) == ActorKindDiscord
}

// normalizeActorKeys returns a map with actor-id keys canonicalized, for
// lookups against maps written with either spelling.
func normalizeActorKeys[V any](m map[string]V) map[string]V {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		if n := NormalizeActorID(k); n != "" {
			out[n] = v
		}
	}
	return out
}
