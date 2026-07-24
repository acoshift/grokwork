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
// long_only = elapsed ≥ threshold OR non-OK outcome (short failures still ping).
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
		if outcome != outcomeOK {
			return true
		}
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

// notifySend posts a notify-done message. Injectable for tests.
type notifySend func(threadID, content string, userIDs []string) error

func discordNotifySend(s *discordgo.Session) notifySend {
	return func(threadID, content string, userIDs []string) error {
		if s == nil {
			return fmt.Errorf("discord session is nil")
		}
		_, err := s.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
			Content: sanitizeDiscordContent(content),
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Users: userIDs,
			},
			Flags: discordgo.MessageFlagsSuppressEmbeds,
		})
		return err
	}
}

// notifyRunDone pings watchers and optionally the run author after a Discord-visible run.
func (b *Bot) notifyRunDone(s *discordgo.Session, threadID, authorID string, result grokrun.Result, elapsed time.Duration) {
	b.notifyRunDoneSend(threadID, authorID, result, elapsed, discordNotifySend(s))
}

// notifyRunFailed is used for pre-Grok failures (worktree/attachments) so watchers
// still get a ping under the same policy as a failed run.
func (b *Bot) notifyRunFailed(s *discordgo.Session, threadID, authorID string, elapsed time.Duration) {
	b.notifyRunDone(s, threadID, authorID, grokrun.Result{Code: 1}, elapsed)
}

// notifyRunDoneSend is the testable core of notifyRunDone (real session lookup + policy).
func (b *Bot) notifyRunDoneSend(threadID, authorID string, result grokrun.Result, elapsed time.Duration, send notifySend) {
	if b == nil || send == nil || threadID == "" || gitworktree.IsWebUnitID(threadID) {
		return
	}
	var watchers []string
	if b.sessions != nil {
		if e, ok := b.sessions.Get(threadID); ok {
			watchers = append([]string(nil), e.WatcherIDs...)
		}
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
	if err := send(threadID, msg, ids); err != nil {
		log.Printf("warn: notify-done thread=%s: %v", threadID, err)
	}
}

// taskAuthorID resolves who queued/started this run for notify policy.
func taskAuthorID(item taskItem, m *discordgo.MessageCreate) string {
	if m != nil && m.Author != nil && m.Author.ID != "" {
		return m.Author.ID
	}
	if item.actor.ID != "" {
		return item.actor.ID
	}
	return item.authorID
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
