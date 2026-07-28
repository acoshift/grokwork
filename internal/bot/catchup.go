package bot

import (
	"cmp"
	"log"
	"slices"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Catch-up sweep: gateway reconnects can lose MESSAGE_CREATE events — a
// rejected RESUME falls back to re-IDENTIFY and Discord never replays the
// gap. Message history is durable over REST, so after every READY/RESUMED we
// re-fetch messages newer than the last provably-healthy moment and feed
// unseen bot-mentions back through onMessage. The handled-message set makes
// gateway replay + sweep idempotent, so over-fetching is always safe.

const (
	// discordEpochMs is the Discord snowflake epoch (2015-01-01T00:00:00Z).
	discordEpochMs = 1420070400000

	catchupOverlap     = time.Minute    // re-fetch margin behind the watermark
	catchupMaxLookback = 6 * time.Hour  // hard cap on the sweep window
	catchupThreadAge   = 72 * time.Hour // session threads younger than this are swept
	catchupMaxTargets  = 50
	catchupPageSize    = 100
	catchupMaxPages    = 3 // per target → max 300 messages
	handledMsgCap      = 4096
)

// advanceCoverage raises the coverage watermark: the latest moment gateway
// event delivery was believed complete. Monotonic; safe from any goroutine.
func (b *Bot) advanceCoverage(t time.Time) {
	if b == nil || t.IsZero() {
		return
	}
	ms := t.UnixMilli()
	for {
		cur := b.coverageMs.Load()
		if ms <= cur {
			return
		}
		if b.coverageMs.CompareAndSwap(cur, ms) {
			return
		}
	}
}

// markMessageHandled records a message ID as processed. Returns false when the
// ID was already recorded (duplicate delivery: RESUME replay or catch-up
// sweep). Bounded FIFO so memory stays flat.
func (b *Bot) markMessageHandled(id string) bool {
	if b == nil || id == "" {
		return true
	}
	b.handledMu.Lock()
	defer b.handledMu.Unlock()
	if b.handledSet == nil {
		b.handledSet = make(map[string]struct{}, handledMsgCap)
	}
	if _, dup := b.handledSet[id]; dup {
		return false
	}
	b.handledSet[id] = struct{}{}
	b.handledQ = append(b.handledQ, id)
	if len(b.handledQ) > handledMsgCap {
		delete(b.handledSet, b.handledQ[0])
		b.handledQ = b.handledQ[1:]
	}
	return true
}

// messageHandled is a read-only peek; onMessage's markMessageHandled remains
// the authoritative check-and-set.
func (b *Bot) messageHandled(id string) bool {
	if b == nil || id == "" {
		return false
	}
	b.handledMu.Lock()
	defer b.handledMu.Unlock()
	_, ok := b.handledSet[id]
	return ok
}

// onResumed fires when discordgo successfully RESUMEs. Discord replays missed
// events on resume, but replay is best-effort — sweep anyway; dedup makes it
// idempotent.
func (b *Bot) onResumed(s *discordgo.Session, _ *discordgo.Resumed) {
	log.Printf("gateway: resumed")
	go b.catchupSweep(s, "resumed")
}

// catchupSweep re-fetches recent messages in mapped channels and active
// session threads, replaying missed bot-mentions through onMessage.
// Single-flight; concurrent triggers (READY + RESUMED) collapse to one sweep.
func (b *Bot) catchupSweep(s *discordgo.Session, reason string) {
	if b == nil || s == nil || b.stopping.Load() {
		return
	}
	if !b.catchupMu.TryLock() {
		// A sweep is already running; have it go one more round so a gap
		// opened after that sweep's start time is still covered.
		b.catchupRerun.Store(true)
		return
	}
	defer b.catchupMu.Unlock()
	for {
		b.catchupSweepLocked(s, reason)
		if b.stopping.Load() || !b.catchupRerun.CompareAndSwap(true, false) {
			return
		}
		reason = "rerun"
	}
}

func (b *Bot) catchupSweepLocked(s *discordgo.Session, reason string) {
	start := time.Now().UTC()
	wm := b.coverageMs.Load()
	if wm <= 0 {
		// First READY of this process: nothing before boot is recoverable.
		b.advanceCoverage(start)
		return
	}
	after := time.UnixMilli(wm).Add(-catchupOverlap)
	if floor := start.Add(-catchupMaxLookback); after.Before(floor) {
		after = floor
	}
	afterID := snowflakeFromTime(after)

	targets := b.catchupTargets()
	if len(targets) >= catchupMaxTargets {
		log.Printf("catchup: target cap reached (%d) — oldest session threads not swept", catchupMaxTargets)
	}
	scanned, recovered := 0, 0
	for _, chID := range targets {
		if b.stopping.Load() {
			break
		}
		n, r := b.catchupChannel(s, chID, afterID)
		scanned += n
		recovered += r
	}
	b.advanceCoverage(start)
	log.Printf("catchup: sweep reason=%s window=%s targets=%d scanned=%d recovered=%d",
		reason, start.Sub(after).Round(time.Second), len(targets), scanned, recovered)
}

// catchupTargets returns the channels worth sweeping: every mapped channel
// plus recently-active session threads, capped to bound REST calls.
// Known gap: threads users created themselves during the outage are not
// listed anywhere we can see cheaply — mentions there stay lost.
func (b *Bot) catchupTargets() []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id == "" || seen[id] || len(out) >= catchupMaxTargets {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if b.cfg != nil {
		for _, ch := range b.cfg.Snapshot().Channels {
			add(ch.ChannelID)
		}
	}
	if b.sessions != nil {
		cutoff := time.Now().UTC().Add(-catchupThreadAge)
		for _, e := range b.sessions.List() { // newest first
			ts, err := time.Parse(time.RFC3339, e.UpdatedAt)
			if err != nil || ts.Before(cutoff) {
				continue
			}
			add(e.ThreadID)
		}
	}
	return out
}

// catchupChannel fetches messages after afterID and replays unseen
// bot-mentions. Pagination is capped; anything beyond the cap is dropped and
// logged rather than silently assumed covered.
func (b *Bot) catchupChannel(s *discordgo.Session, channelID, afterID string) (scanned, recovered int) {
	guildID := channelGuildID(s, channelID)
	if guildID == "" {
		log.Printf("catchup: skip channel %s: unknown guild", channelID)
		return 0, 0
	}
	after := afterID
	for page := 0; ; page++ {
		if page >= catchupMaxPages {
			log.Printf("catchup: channel %s: page cap reached (%d×%d msgs) — older backlog skipped",
				channelID, catchupMaxPages, catchupPageSize)
			return
		}
		batch, err := b.fetchMessagesAfter(s, channelID, after)
		if err != nil {
			log.Printf("catchup: fetch channel %s: %v", channelID, err)
			return
		}
		if len(batch) == 0 {
			return
		}
		sortMessagesByID(batch)
		for _, msg := range batch {
			if msg == nil || msg.Author == nil {
				continue
			}
			scanned++
			if msg.Author.Bot {
				continue
			}
			// REST message objects omit guild_id; onMessage requires it.
			msg.GuildID = guildID
			mc := &discordgo.MessageCreate{Message: msg}
			if s.State == nil || s.State.User == nil || !mentionsUser(mc, s.State.User.ID) {
				continue
			}
			if b.messageHandled(msg.ID) {
				continue
			}
			recovered++
			log.Printf("catchup: replaying missed message id=%s channel=%s author=%s",
				msg.ID, channelID, msg.Author.ID)
			b.dispatchCatchup(s, mc)
		}
		after = batch[len(batch)-1].ID
		if len(batch) < catchupPageSize {
			return
		}
	}
}

func (b *Bot) fetchMessagesAfter(s *discordgo.Session, channelID, afterID string) ([]*discordgo.Message, error) {
	if b.catchupFetch != nil {
		return b.catchupFetch(channelID, afterID)
	}
	return s.ChannelMessages(channelID, catchupPageSize, "", afterID, "")
}

func (b *Bot) dispatchCatchup(s *discordgo.Session, mc *discordgo.MessageCreate) {
	if b.catchupDispatch != nil {
		b.catchupDispatch(s, mc)
		return
	}
	b.onMessage(s, mc)
}

// channelGuildID resolves a channel's guild via state, falling back to REST.
func channelGuildID(s *discordgo.Session, channelID string) string {
	ch, err := s.State.Channel(channelID)
	if err != nil || ch == nil {
		ch, err = s.Channel(channelID)
		if err != nil || ch == nil {
			return ""
		}
	}
	return ch.GuildID
}

// snowflakeFromTime builds the smallest snowflake ID for t, for use as an
// `after` cursor in message-history fetches.
func snowflakeFromTime(t time.Time) string {
	ms := t.UnixMilli() - discordEpochMs
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// sortMessagesByID orders a REST batch oldest-first so replayed tasks queue in
// send order (the API does not guarantee ordering with `after`).
func sortMessagesByID(msgs []*discordgo.Message) {
	slices.SortFunc(msgs, func(a, b *discordgo.Message) int {
		return cmp.Compare(snowflakeUint(a), snowflakeUint(b))
	})
}

func snowflakeUint(m *discordgo.Message) uint64 {
	if m == nil {
		return 0
	}
	n, err := strconv.ParseUint(m.ID, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
