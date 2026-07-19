package bot

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// boardDigestInterval is how often the optional nightly digest posts.
const boardDigestInterval = 24 * time.Hour

var boardDigestOnce sync.Once

func (b *Bot) startBoardDigest(s *discordgo.Session) {
	boardDigestOnce.Do(func() {
		ch := ""
		if b != nil && b.cfg != nil {
			ch = b.cfg.BoardDigestChannelValue()
		}
		if ch == "" {
			log.Printf("bg: board digest disabled (boardDigestChannel unset)")
		} else {
			log.Printf("bg: starting board digest channel=%s interval=%s initial_delay=2m", ch, boardDigestInterval)
		}
		go b.runBoardDigest(s)
	})
}

func (b *Bot) runBoardDigest(s *discordgo.Session) {
	// Brief delay so gateway ready isn't competing with the first post.
	time.Sleep(2 * time.Minute)
	// First fire after the initial delay only if a channel is configured;
	// subsequent fires are on the 24h ticker (nightly-ish for long-running hosts).
	b.runBoardDigestCycle(s, "initial")

	ticker := time.NewTicker(boardDigestInterval)
	defer ticker.Stop()
	for range ticker.C {
		b.runBoardDigestCycle(s, "tick")
	}
}

func (b *Bot) runBoardDigestCycle(s *discordgo.Session, reason string) {
	if b == nil || b.cfg == nil || s == nil {
		return
	}
	channelID := strings.TrimSpace(b.cfg.BoardDigestChannelValue())
	if channelID == "" {
		log.Printf("bg: board digest skipped reason=%s (channel unset)", reason)
		return
	}

	staleDays := b.boardStaleDays()
	rows := b.collectBoardRows("", "", "", false, staleDays, time.Now())
	// Skip empty "nothing happening" noise on the initial post; still post on
	// scheduled ticks so leads see a clear zero-state.
	if reason == "initial" && len(rows) == 0 {
		log.Printf("bg: board digest skipped reason=%s (no active threads)", reason)
		return
	}

	body := formatBoardCard(rows, "", "", "", false, staleDays)
	// Prefix so channel history is greppable.
	msg := "**Nightly team board**\n" + body
	if _, err := discordSend(s, channelID, msg); err != nil {
		log.Printf("bg: board digest post failed reason=%s channel=%s: %v", reason, channelID, err)
		return
	}
	log.Printf("bg: board digest posted reason=%s channel=%s threads=%d", reason, channelID, len(rows))
}
