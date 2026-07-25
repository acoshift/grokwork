package bot

import (
	"strings"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// hasDiscordSurface reports whether unit has a Discord thread to render into.
//
// This is the single question every Discord-rendering path should ask. It
// replaced a dozen scattered gitworktree.IsWebUnitID checks, which answered the
// same question by sniffing the shape of a unit id — so adding a surface, or a
// Discord unit that is not keyed by its thread id, meant finding all of them.
//
// Note what this does NOT answer: whether Discord is reachable right now. A
// thread whose writes are failing is a degraded Discord unit, not a web-native
// one, and callers that conflate the two send a web unit's notification into a
// channel that does not exist. Gate rendering on this AND on a live session.
func (b *Bot) hasDiscordSurface(unitID string) bool {
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return false
	}
	if b != nil && b.sessions != nil {
		if e, ok := b.sessions.Get(unitID); ok {
			return e.HasDiscord()
		}
	}
	// No entry yet — an early ack before the session is written, or a poller
	// racing creation. Fall back to the id shape; this is the only place in bot
	// allowed to, and it agrees with what the store would have derived anyway
	// (sessionstore.ensureDiscordRef uses the same rule).
	return !gitworktree.IsWebUnitID(unitID)
}
