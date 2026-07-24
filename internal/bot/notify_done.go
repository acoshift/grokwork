package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// runOutcome classifies a finished Grok run for notify policy.
type runOutcome int

const (
	outcomeOK runOutcome = iota
	outcomeError
	outcomeCancelled
)

func classifyRunOutcome(result grokrun.Result) runOutcome {
	if result.Cancelled {
		return outcomeCancelled
	}
	if result.Code != 0 || result.MaxTurnsReached {
		return outcomeError
	}
	return outcomeOK
}

func outcomeLabel(o runOutcome) string {
	switch o {
	case outcomeCancelled:
		return "cancelled"
	case outcomeError:
		return "failed"
	default:
		return "done"
	}
}

// shouldNotifyAuthor reports whether the run author should be @mentioned.
func shouldNotifyAuthor(mode string, longMs int, outcome runOutcome, elapsed time.Duration) bool {
	mode = config.NormalizeNotifyOnDone(mode)
	switch mode {
	case config.NotifyOnDoneNever:
		return false
	case config.NotifyOnDoneAlways:
		return true
	case config.NotifyOnDoneErrors:
		return outcome != outcomeOK
	case config.NotifyOnDoneLongOnly:
		if longMs <= 0 {
			longMs = config.DefaultNotifyOnDoneLongMs
		}
		return elapsed >= time.Duration(longMs)*time.Millisecond
	default:
		return outcome != outcomeOK
	}
}

// notifyMentionIDs builds the set of Discord user ids to @mention after a run.
// Watchers always get mentioned (they opted in). The author is included when
// notifyOnDone policy says so. Deduped; empty author/watchers skipped.
func notifyMentionIDs(authorID string, watchers []string, mode string, longMs int, outcome runOutcome, elapsed time.Duration) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range watchers {
		add(id)
	}
	if shouldNotifyAuthor(mode, longMs, outcome, elapsed) {
		add(authorID)
	}
	return out
}

func formatNotifyDoneMessage(ids []string, outcome runOutcome, elapsed time.Duration) string {
	if len(ids) == 0 {
		return ""
	}
	mentions := make([]string, 0, len(ids))
	for _, id := range ids {
		mentions = append(mentions, "<@"+id+">")
	}
	label := outcomeLabel(outcome)
	return fmt.Sprintf("%s — run **%s** · %s", strings.Join(mentions, " "), label, formatElapsed(elapsed))
}

// notifyRunDone pings watchers and optionally the run author after a Discord-visible run.
func (b *Bot) notifyRunDone(s *discordgo.Session, threadID, authorID string, result grokrun.Result, elapsed time.Duration) {
	if b == nil || s == nil || threadID == "" || gitworktree.IsWebUnitID(threadID) {
		return
	}
	var watchers []string
	if e, ok := b.sessions.Get(threadID); ok {
		watchers = append([]string(nil), e.WatcherIDs...)
	}
	mode := config.DefaultNotifyOnDone
	longMs := config.DefaultNotifyOnDoneLongMs
	if b.cfg != nil {
		mode = b.cfg.NotifyOnDoneValue()
		longMs = b.cfg.NotifyOnDoneLongMsValue()
	}
	outcome := classifyRunOutcome(result)
	ids := notifyMentionIDs(authorID, watchers, mode, longMs, outcome, elapsed)
	if len(ids) == 0 {
		return
	}
	msg := formatNotifyDoneMessage(ids, outcome, elapsed)
	if msg == "" {
		return
	}
	if _, err := s.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
		Content: sanitizeDiscordContent(msg),
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Users: ids,
		},
		Flags: discordgo.MessageFlagsSuppressEmbeds,
	}); err != nil {
		log.Printf("warn: notify-done thread=%s: %v", threadID, err)
	}
}

// preserveWatcherFields copies watch list across session Set rebuilds.
func preserveWatcherFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if len(next.WatcherIDs) == 0 && len(prev.WatcherIDs) > 0 {
		next.WatcherIDs = append([]string(nil), prev.WatcherIDs...)
	}
}
