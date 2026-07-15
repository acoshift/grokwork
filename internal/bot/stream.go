package bot

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const (
	streamEditMinInterval = 1500 * time.Millisecond
	streamLiveBudget      = 1800 // leave room for "_(streaming…)_"
)

type streamPoster struct {
	s         *discordgo.Session
	channelID string

	mu       sync.Mutex
	full     strings.Builder
	msgID    string
	lastEdit time.Time
	closed   bool
}

func newStreamPoster(s *discordgo.Session, channelID string) *streamPoster {
	return &streamPoster{s: s, channelID: channelID}
}

func (p *streamPoster) OnDelta(delta string) {
	if delta == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.full.WriteString(delta)
	if p.msgID == "" || time.Since(p.lastEdit) >= streamEditMinInterval {
		p.flushLocked(false)
	}
}

func (p *streamPoster) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.flushLocked(false)
}

// Finish returns true if the full reply already lives in the stream message.
func (p *streamPoster) Finish() (streamedFully bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	text := p.full.String()
	if text == "" {
		return false
	}
	if p.msgID == "" {
		return false
	}
	if len(text) <= maxMsg {
		if _, err := p.s.ChannelMessageEdit(p.channelID, p.msgID, text); err != nil {
			log.Printf("warn: stream final edit: %v", err)
			return false
		}
		return true
	}
	note := fmt.Sprintf("(stream preview — full reply follows, %d chars)", len(text))
	preview := streamPreview(text, maxMsg-len(note)-2) + "\n\n" + note
	if _, err := p.s.ChannelMessageEdit(p.channelID, p.msgID, preview); err != nil {
		log.Printf("warn: stream final preview edit: %v", err)
	}
	return false
}

func (p *streamPoster) Text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.full.String()
}

func (p *streamPoster) flushLocked(final bool) {
	text := p.full.String()
	if text == "" {
		return
	}
	content := text
	if !final {
		content = streamPreview(text, streamLiveBudget)
		if len(text) > streamLiveBudget {
			content += "\n\n_(streaming…)_"
		} else {
			content += "\n\n_(streaming…)_"
		}
		if len(content) > maxMsg {
			content = streamPreview(text, maxMsg-20) + "\n\n_(streaming…)_"
		}
	} else if len(content) > maxMsg {
		content = streamPreview(text, maxMsg)
	}

	var err error
	if p.msgID == "" {
		var msg *discordgo.Message
		msg, err = p.s.ChannelMessageSend(p.channelID, content)
		if err == nil {
			p.msgID = msg.ID
		}
	} else {
		_, err = p.s.ChannelMessageEdit(p.channelID, p.msgID, content)
	}
	if err != nil {
		log.Printf("warn: stream post/edit channel=%s: %v", p.channelID, err)
		return
	}
	p.lastEdit = time.Now()
}

func streamPreview(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(s) <= budget {
		return s
	}
	cut := budget
	if cut > len(s) {
		cut = len(s)
	}
	chunk := s[:cut]
	if i := strings.LastIndex(chunk, "\n"); i > budget/2 {
		chunk = chunk[:i]
	}
	for !utf8.ValidString(chunk) && len(chunk) > 0 {
		chunk = chunk[:len(chunk)-1]
	}
	return strings.TrimRight(chunk, " \t") + "…"
}

type thoughtTracker struct {
	mu   sync.Mutex
	buf  strings.Builder
	last string
}

func (t *thoughtTracker) OnDelta(delta string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.WriteString(delta)
	s := t.buf.String()
	if len(s) > 400 {
		s = s[len(s)-400:]
		t.buf.Reset()
		t.buf.WriteString(s)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = "…" + s[len(s)-79:]
	}
	t.last = s
}

func (t *thoughtTracker) Latest() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}
