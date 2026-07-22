package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleQueue(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /queue` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply queue-not-thread: %v", err)
		}
		return
	}
	st := b.stateFor(m.ChannelID)
	st.mu.Lock()
	defer st.mu.Unlock()
	var lines []string
	if st.job != nil {
		lines = append(lines, "**Running** · active task")
	} else {
		lines = append(lines, "**Idle** · no active run")
	}
	if len(st.queue) == 0 {
		lines = append(lines, "Queue empty.")
	} else {
		lines = append(lines, fmt.Sprintf("**Queue** (%d):", len(st.queue)))
		for i, it := range st.queue {
			who := it.authorName
			if who == "" {
				who = it.actor.DisplayName
			}
			if who == "" {
				who = it.authorID
			}
			if who == "" {
				who = "?"
			}
			intent := it.intentPreview
			if intent == "" {
				intent = intentPreview(it.parsed.Prompt, 80)
			}
			mode := it.snapMode
			if mode == "" {
				mode = "fix"
			}
			lines = append(lines, fmt.Sprintf("%d. **%s** · `%s` · %s", i+1, who, mode, intent))
		}
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, strings.Join(lines, "\n"), ref(m)); err != nil {
		log.Printf("error: reply queue: %v", err)
	}
}

func (b *Bot) handleDequeue(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /dequeue N` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply dequeue-not-thread: %v", err)
		}
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(parsed.Arg))
	if err != nil || n < 1 {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Usage: `@Grok /dequeue N` (1-based index from `/queue`).", ref(m)); err != nil {
			log.Printf("error: reply dequeue-usage: %v", err)
		}
		return
	}
	uid := ""
	if m.Author != nil {
		uid = m.Author.ID
	}
	st := b.stateFor(m.ChannelID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if n > len(st.queue) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, fmt.Sprintf("No queue item **%d** (queue len %d).", n, len(st.queue)), ref(m)); err != nil {
			log.Printf("error: reply dequeue-miss: %v", err)
		}
		return
	}
	idx := n - 1
	it := st.queue[idx]
	// Owner/mod or own item.
	can := it.authorID != "" && it.authorID == uid
	if !can {
		if e, ok := b.sessions.Get(m.ChannelID); ok {
			can = b.canControlThread(s, m, e)
		} else {
			can = b.isModerator(s, m)
		}
	}
	if !can {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "You can only dequeue your own items (or owner/mod).", ref(m)); err != nil {
			log.Printf("error: reply dequeue-deny: %v", err)
		}
		return
	}
	oldID := it.taskID
	st.queue = append(st.queue[:idx], st.queue[idx+1:]...)
	if err := b.saveJournalFromState(m.ChannelID, st, taskItem{}, false); err != nil {
		log.Printf("warn: journal dequeue: %v", err)
	}
	if b.runs != nil && oldID != "" {
		b.runs.RemoveTaskFiles(m.ChannelID, oldID)
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, fmt.Sprintf("Removed queue item **%d**.", n), ref(m)); err != nil {
		log.Printf("error: reply dequeue-ok: %v", err)
	}
}

func (b *Bot) handleCancelMine(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /cancel-mine` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply cancel-mine-not-thread: %v", err)
		}
		return
	}
	uid := ""
	if m.Author != nil {
		uid = m.Author.ID
	}
	if uid == "" {
		return
	}
	st := b.stateFor(m.ChannelID)
	st.mu.Lock()
	defer st.mu.Unlock()
	var kept []taskItem
	var removed int
	for _, it := range st.queue {
		if it.authorID == uid || it.actor.ID == uid {
			removed++
			if b.runs != nil && it.taskID != "" {
				b.runs.RemoveTaskFiles(m.ChannelID, it.taskID)
			}
			continue
		}
		kept = append(kept, it)
	}
	st.queue = kept
	if removed > 0 {
		if err := b.saveJournalFromState(m.ChannelID, st, taskItem{}, false); err != nil {
			log.Printf("warn: journal cancel-mine: %v", err)
		}
	}
	if _, err := s.ChannelMessageSendReply(m.ChannelID, fmt.Sprintf("Removed **%d** of your queued item(s).", removed), ref(m)); err != nil {
		log.Printf("error: reply cancel-mine: %v", err)
	}
}

// queueSnapshot returns a copy of queue items for tests.
func (b *Bot) queueSnapshot(threadID string) []taskItem {
	st := b.stateFor(threadID)
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]taskItem(nil), st.queue...)
}
