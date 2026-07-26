package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// handleWatch implements @Grok /watch — opt into @mention when runs on this thread finish.
func (b *Bot) handleWatch(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := discordReply(s, m.ChannelID, "Use `@Grok /watch` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply watch-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	if _, ok := b.sessions.Get(m.ChannelID); !ok {
		if _, err := discordReply(s, m.ChannelID, "No session for this thread yet. Start a task first, then `/watch`.", ref(m)); err != nil {
			log.Printf("error: reply watch-no-session: %v", err)
		}
		return
	}
	wasWatching := false
	if e0, ok0 := b.sessions.Get(m.ChannelID); ok0 {
		wasWatching = e0.IsWatcher(m.Author.ID)
	}
	var added bool
	e, ok, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		added = ent.AddWatcher(m.Author.ID)
	})
	// A vanished unit (Patch found nothing) is a failure, not a silent success:
	// nobody was added, so the row must not claim otherwise.
	b.auditCmdMsg(audit.ActionSessionWatch, m, e.Project, watchAuditErr(err, ok), map[string]any{
		"added":    added,
		"watchers": len(e.WatcherIDs),
	})
	if err != nil {
		if _, sendErr := discordReply(s, m.ChannelID, "Could not update watch list: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply watch-save: %v", sendErr)
		}
		return
	}
	if !ok {
		if _, err := discordReply(s, m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply watch-missing: %v", err)
		}
		return
	}
	var msg string
	switch {
	case added:
		msg = fmt.Sprintf(
			"Watching this thread (%d watcher(s)). You will be @mentioned when each run completes or fails until `@Grok /unwatch`.",
			len(e.WatcherIDs),
		)
	case wasWatching:
		msg = "Already watching this thread — you stay on the list for every run until `@Grok /unwatch`."
	default:
		msg = fmt.Sprintf(
			"Could not add you as a watcher (limit is %d). Ask someone to `/unwatch` or wait.",
			sessionstore.MaxWatchers,
		)
	}
	if _, err := discordReply(s, m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply watch-ok: %v", err)
	}
}

// handleUnwatch implements @Grok /unwatch.
func (b *Bot) handleUnwatch(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := discordReply(s, m.ChannelID, "Use `@Grok /unwatch` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	if _, ok := b.sessions.Get(m.ChannelID); !ok {
		if _, err := discordReply(s, m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-no-session: %v", err)
		}
		return
	}
	var removed bool
	e, ok, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		removed = ent.RemoveWatcher(m.Author.ID)
	})
	b.auditCmdMsg(audit.ActionSessionUnwatch, m, e.Project, watchAuditErr(err, ok), map[string]any{
		"removed":  removed,
		"watchers": len(e.WatcherIDs),
	})
	if err != nil {
		if _, sendErr := discordReply(s, m.ChannelID, "Could not update watch list: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply unwatch-save: %v", sendErr)
		}
		return
	}
	if !ok {
		if _, err := discordReply(s, m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-missing: %v", err)
		}
		return
	}
	msg := "You were not watching this thread."
	if removed {
		msg = "Stopped watching. You will no longer be @mentioned when runs finish."
	}
	if _, err := discordReply(s, m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply unwatch-ok: %v", err)
	}
}

// watchAuditErr reports the Patch outcome as one error: a store failure, or a
// unit that no longer exists between the guard above and the write.
func watchAuditErr(err error, found bool) error {
	if err != nil {
		return err
	}
	if !found {
		return errors.New("unknown work unit")
	}
	return nil
}

func formatWatcherStatusLine(e sessionstore.Entry) string {
	if len(e.WatcherIDs) == 0 {
		return "**watchers:** (none — `@Grok /watch`)"
	}
	parts := make([]string, 0, len(e.WatcherIDs))
	for _, id := range e.WatcherIDs {
		parts = append(parts, "<@"+id+">")
	}
	return "**watchers:** " + strings.Join(parts, ", ")
}
