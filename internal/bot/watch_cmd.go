package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// handleWatch implements @Grok /watch — opt into @mention when runs on this thread finish.
func (b *Bot) handleWatch(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /watch` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply watch-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	if _, ok := b.sessions.Get(m.ChannelID); !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet. Start a task first, then `/watch`.", ref(m)); err != nil {
			log.Printf("error: reply watch-no-session: %v", err)
		}
		return
	}
	var added bool
	e, ok, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		added = ent.AddWatcher(m.Author.ID)
	})
	if err != nil {
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not update watch list: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply watch-save: %v", sendErr)
		}
		return
	}
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply watch-missing: %v", err)
		}
		return
	}
	msg := "Already watching this thread — you will be mentioned when a run completes or fails."
	if added {
		msg = fmt.Sprintf("Watching this thread (%d watcher(s)). You will be @mentioned once when a run completes or fails. `@Grok /unwatch` to stop.", len(e.WatcherIDs))
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply watch-ok: %v", err)
	}
}

// handleUnwatch implements @Grok /unwatch.
func (b *Bot) handleUnwatch(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /unwatch` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	if _, ok := b.sessions.Get(m.ChannelID); !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-no-session: %v", err)
		}
		return
	}
	var removed bool
	_, ok, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		removed = ent.RemoveWatcher(m.Author.ID)
	})
	if err != nil {
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not update watch list: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply unwatch-save: %v", sendErr)
		}
		return
	}
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
			log.Printf("error: reply unwatch-missing: %v", err)
		}
		return
	}
	msg := "You were not watching this thread."
	if removed {
		msg = "Stopped watching. You will no longer be @mentioned when runs finish."
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply unwatch-ok: %v", err)
	}
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
