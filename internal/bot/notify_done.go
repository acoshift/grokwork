package bot

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
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

// notifyRunDone pings watchers and optionally the run author after a run.
//
// A Discord thread gets one in-thread message that @mentions everyone; a
// web-native unit has no channel to post into, so each recipient is DMed instead.
// The two cannot share a message: a DM addressed to one person must not read
// "@you @someone-else — run done", and it carries no ambient context, so it has to
// name the work and link to it.
func (b *Bot) notifyRunDone(s *discordgo.Session, threadID, authorID string, result grokrun.Result, elapsed time.Duration) {
	if !b.hasDiscordSurface(threadID) {
		b.notifyRunDoneDM(threadID, authorID, result, elapsed, discordDMSend(s))
		return
	}
	b.notifyRunDoneSend(threadID, authorID, result, elapsed, discordNotifySend(s))
}

// notifyRunFailed is used for pre-Grok failures (worktree/attachments) so watchers
// still get a ping under the same policy as a failed run.
func (b *Bot) notifyRunFailed(s *discordgo.Session, threadID, authorID string, elapsed time.Duration) {
	b.notifyRunDone(s, threadID, authorID, grokrun.Result{Code: 1}, elapsed)
}

// notifyRecipients resolves who to ping for a finished run: watchers (who opted
// in) plus the author when notifyOnDone policy says so. Policy only — no delivery.
func (b *Bot) notifyRecipients(threadID, authorID string, outcome runOutcome, elapsed time.Duration) []string {
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
	return notifyMentionIDs(authorID, watchers, mode, longMs, outcome, elapsed)
}

// notifyRunDoneSend is the testable core of the in-thread ping (session lookup +
// policy). Keeps refusing web-unit ids: those are not Discord channels, and posting
// to one is a guaranteed 4xx every poll.
func (b *Bot) notifyRunDoneSend(threadID, authorID string, result grokrun.Result, elapsed time.Duration, send notifySend) {
	if b == nil || send == nil || threadID == "" || !b.hasDiscordSurface(threadID) {
		return
	}
	outcome := classifyRunOutcome(result)
	ids := b.notifyRecipients(threadID, authorID, outcome, elapsed)
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

// maxNotifyDMs caps the fan-out of one finished run. Reached only via a long
// watcher list; the drop is logged rather than silent.
const maxNotifyDMs = 10

// dmSend delivers one direct message to a user. Injectable for tests.
type dmSend func(userID, content string) error

// notifyRunDoneDM is notifyRunDoneSend for web-native units: same policy, but one
// DM per recipient because there is no shared channel.
func (b *Bot) notifyRunDoneDM(threadID, authorID string, result grokrun.Result, elapsed time.Duration, dm dmSend) {
	if b == nil || dm == nil || threadID == "" {
		return
	}
	outcome := classifyRunOutcome(result)
	ids := b.notifyRecipients(threadID, authorID, outcome, elapsed)
	// A web session's actor id is a Discord snowflake only when the viewer logged in
	// through Discord OAuth. Anyone else cannot be DMed — but "cannot push to them"
	// must not mean "never tell them", which is what dropping the id did. They get
	// an inbox entry instead, and only Discord-shaped ids go on to the DM fan-out.
	var unreachable []string
	ids = slices.DeleteFunc(ids, func(id string) bool {
		if looksLikeDiscordUserID(id) {
			return false
		}
		unreachable = append(unreachable, id)
		return true
	})
	if len(unreachable) > 0 {
		b.deliverInbox(unreachable, threadID, outcome, elapsed)
	}
	if len(ids) == 0 {
		return
	}
	if len(ids) > maxNotifyDMs {
		log.Printf("notify-done: unit=%s recipients=%d capped to %d", threadID, len(ids), maxNotifyDMs)
		ids = ids[:maxNotifyDMs]
	}
	msg := b.formatNotifyDoneDM(threadID, outcome, elapsed)
	if msg == "" {
		return
	}
	for _, id := range ids {
		// One recipient blocking bot DMs must not silence the rest.
		if err := dm(id, msg); err != nil {
			log.Printf("warn: notify-done dm unit=%s user=%s: %v", threadID, id, err)
		}
	}
}

// formatNotifyDoneDM writes the DM body. Unlike the in-thread message it names the
// work and links to it: a DM arrives with no surrounding context, so "run done" on
// its own is unactionable.
func (b *Bot) formatNotifyDoneDM(threadID string, outcome runOutcome, elapsed time.Duration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Run **%s** · %s", outcomeLabel(outcome), formatElapsed(elapsed))
	if b.sessions != nil {
		if e, ok := b.sessions.Get(threadID); ok {
			// Project name and goal are already Discord-visible (brief cards); never
			// add cwd/branch, which would leak a local path.
			if p := strings.TrimSpace(e.Project); p != "" {
				fmt.Fprintf(&sb, " · %s", p)
			}
			if g := strings.TrimSpace(e.Goal); g != "" {
				fmt.Fprintf(&sb, "\n%s", truncateRunes(g, 200))
			}
		}
	}
	if u := b.sessionWebURL(threadID); u != "" {
		sb.WriteString("\n" + u)
	} else {
		// Without webPublicBaseURL there is no link to give, so at least name the unit.
		sb.WriteString("\nSession `" + threadID + "` (set webPublicBaseURL for a direct link)")
	}
	return sb.String()
}

// looksLikeDiscordUserID reports whether id is shaped like a snowflake.
func looksLikeDiscordUserID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) < 17 || len(id) > 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// discordDMSend opens (or reuses) the recipient's DM channel and posts there.
func discordDMSend(s *discordgo.Session) dmSend {
	return func(userID, content string) error {
		if s == nil {
			return fmt.Errorf("discord session is nil")
		}
		ch, err := s.UserChannelCreate(userID)
		if err != nil {
			return fmt.Errorf("open dm: %w", err)
		}
		_, err = s.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
			Content: sanitizeDiscordContent(content),
			// Addressed to one person: nothing to ping, and never let content parse into one.
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
			Flags:           discordgo.MessageFlagsSuppressEmbeds,
		})
		return err
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
