package sessionstore

import "strings"

// DiscordRef holds the Discord coordinates of a unit that has a Discord surface.
// Nil means the unit is web-native: there is no thread to render into, and code
// must not try (posting to a synthetic unit id is a guaranteed 4xx).
//
// Presence of this struct — not the shape of the unit id — is the answer to
// "does this unit have a Discord surface?". See Entry.HasDiscord.
type DiscordRef struct {
	ThreadID string `json:"threadId,omitempty"`
	URL      string `json:"url,omitempty"` // jump link when known
}

// HasDiscord reports whether this unit has a Discord thread to render into.
//
// This is the single predicate for surface dispatch. It deliberately does not
// consider whether the gateway is currently up: a thread that exists while
// Discord is refusing writes is a *degraded* Discord unit, which is a different
// case from a web-native unit that never had a thread, and the two must not
// collapse (a web unit's finished-run ping goes to DMs; a degraded thread's is
// skipped rather than redirected).
func (e Entry) HasDiscord() bool {
	return e.Discord != nil && strings.TrimSpace(e.Discord.ThreadID) != ""
}

// webUnitIDPrefix marks a web-native unit id. Canonical definition lives in
// gitworktree.IsWebUnitID; duplicated here as a bare prefix test to keep the
// store from depending on the git layer for one string check.
// TestWebUnitKeyMatchesGitworktree pins the two against each other.
const webUnitIDPrefix = "w_"

// isWebUnitKey reports whether a store key is a synthetic web-native unit id
// rather than a Discord thread id.
func isWebUnitKey(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), webUnitIDPrefix)
}

// ensureDiscordRef derives Entry.Discord from the store key.
//
// This is the ONE place that infers a surface from the shape of a unit id, and
// it exists because legacy data has no other reliable signal: Origin=="web"
// means "started from the web UI", which is *also* true of a web-started run
// that did open a Discord thread (StartWebTask), so it cannot be used here —
// misreading it would strip a real thread of its cards.
//
// Called from load (migrating existing sessions.json) and from Set/Patch, so
// every entry is normalized regardless of which creation site wrote it.
func ensureDiscordRef(unitID string, e *Entry) {
	if e == nil || strings.TrimSpace(unitID) == "" || isWebUnitKey(unitID) {
		return
	}
	if e.Discord == nil {
		e.Discord = &DiscordRef{}
	}
	if e.Discord.ThreadID == "" {
		e.Discord.ThreadID = unitID
	}
	// Legacy flat field is the source on first migration; mirrored back after so a
	// previous binary reading the same file still finds the jump link.
	if e.Discord.URL == "" {
		e.Discord.URL = e.DiscordURL
	}
	if e.DiscordURL == "" {
		e.DiscordURL = e.Discord.URL
	}
}
