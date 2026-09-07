package bot

import (
	"cmp"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const (
	slaNotifyInterval     = 30 * time.Second
	slaNotifyInitialDelay = 30 * time.Second
)

type slaClockPass struct {
	name    string
	clock   SLAClock
	alerted string
}

func (b *Bot) startSLAInboxSweep() {
	if b == nil {
		return
	}
	b.slaNotifyOnce.Do(func() {
		log.Printf("bg: starting SLA inbox sweeper interval=%s initial_delay=%s",
			slaNotifyInterval, slaNotifyInitialDelay)
		go b.runSLAInboxSweep()
	})
}

func (b *Bot) runSLAInboxSweep() {
	ctx := b.bgContext()
	log.Printf("bg: SLA inbox sweeper running (waiting %s before first sweep)", slaNotifyInitialDelay)
	if !sleepCtx(ctx, slaNotifyInitialDelay) {
		log.Printf("bg: SLA inbox sweeper stopped before first sweep")
		return
	}
	b.sweepSLAInbox()

	ticker := time.NewTicker(slaNotifyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("bg: SLA inbox sweeper stopped")
			return
		case <-ticker.C:
			b.sweepSLAInbox()
		}
	}
}

func (b *Bot) sweepSLAInbox() int {
	return b.sweepSLAInboxAt(time.Now(), discordNotifySend(b.Discord()))
}

// sweepSLAInboxAt walks open cases at a frozen instant and inbox-pings involved
// actors for clocks that have newly breached. send nil skips Discord.
func (b *Bot) sweepSLAInboxAt(now time.Time, send notifySend) int {
	if b == nil || b.inbox == nil || b.sessions == nil {
		return 0
	}
	appended := 0
	for _, listed := range b.sessions.List() {
		appended += b.sweepOneSLACase(listed.ThreadID, listed.Entry, now, send)
	}
	return appended
}

func (b *Bot) sweepOneSLACase(threadID string, e sessionstore.Entry, now time.Time, send notifySend) int {
	if !e.IsCase() || e.IsCaseClosed() {
		return 0
	}
	sla := b.caseSLAAt(e, now)
	if !sla.Breached {
		return 0
	}
	start, ok := e.SLARoundStart()
	if !ok {
		return 0
	}
	recipients := slaRecipients(e)
	if len(recipients) == 0 {
		log.Printf("sla-notify: thread=%s project=%s breached with no involved actor", threadID, e.Project)
		return 0
	}

	clocks := []slaClockPass{
		{"first response", sla.FirstResponse, e.SLAAlertedFirstResponse},
		{"resolution", sla.Resolution, e.SLAAlertedResolution},
	}

	appended := 0
	var delivered []slaClockPass
	for _, c := range clocks {
		if !c.clock.Breached || slaAlreadyAlerted(c.alerted, start) {
			continue
		}
		n, ok := b.appendSLAClock(threadID, e, recipients, c.name, c.clock)
		appended += n
		if ok {
			delivered = append(delivered, c)
		}
	}
	if len(delivered) == 0 {
		return appended
	}

	if send != nil && b.hasDiscordSurface(threadID) {
		if err := send(threadID, formatSLADiscord(b.discordMentionIDs(recipients), delivered, e), slaMentionUsers(b, recipients)); err != nil {
			log.Printf("warn: sla-notify discord thread=%s: %v", threadID, err)
		}
	}

	key := start.UTC().Format(time.RFC3339)
	if _, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		for _, c := range delivered {
			switch c.name {
			case "first response":
				ent.SLAAlertedFirstResponse = key
			case "resolution":
				ent.SLAAlertedResolution = key
			}
		}
	}); err != nil {
		log.Printf("warn: sla-notify watermark thread=%s: %v", threadID, err)
	}
	return appended
}

func (b *Bot) appendSLAClock(threadID string, e sessionstore.Entry, recipients []string, name string, clock SLAClock) (int, bool) {
	subject := slaClockPhrase(name, clock)
	if suffix := cmp.Or(strings.TrimSpace(e.CaseKey), strings.TrimSpace(e.Project)); suffix != "" {
		subject += " · " + suffix
	}
	item := inbox.Item{
		Kind:    inbox.KindSLABreached,
		Subject: subject,
		Body:    truncateRunes(slaNotifyTitle(e), 200),
		URL:     inboxSessionPath(threadID, e.Project),
		UnitID:  threadID,
		Project: e.Project,
	}
	n := 0
	for _, id := range recipients {
		if _, err := b.inbox.Append(id, item); err != nil {
			log.Printf("warn: sla-notify append actor=%s unit=%s: %v", id, threadID, err)
			continue
		}
		n++
	}
	return n, n > 0
}

func slaAlreadyAlerted(watermark string, start time.Time) bool {
	alerted, ok := sessionstore.ParseStamp(watermark)
	return ok && alerted.Equal(start)
}

func slaClockPhrase(name string, clock SLAClock) string {
	return "SLA · " + name + " " + formatCoarseDuration(clock.Over()) + " over"
}

func slaNotifyTitle(e sessionstore.Entry) string {
	if e.IsCase() {
		if t := strings.TrimSpace(e.CustomerTitle); t != "" {
			return t
		}
	}
	return strings.TrimSpace(e.Goal)
}

func slaRecipients(e sessionstore.Entry) []string {
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || slices.Contains(out, id) {
			return
		}
		out = append(out, id)
	}
	add(e.OwnerID)
	add(e.EngineerID)
	for _, id := range e.CoOwnerIDs {
		add(id)
	}
	for _, id := range e.WatcherIDs {
		add(id)
	}
	return out
}

func slaMentionUsers(b *Bot, ids []string) []string {
	var out []string
	for _, id := range ids {
		snow, ok := b.discordDMTarget(id)
		if !ok || !looksLikeDiscordUserID(snow) || slices.Contains(out, snow) {
			continue
		}
		out = append(out, snow)
	}
	return out
}

func formatSLADiscord(mentionIDs []string, delivered []slaClockPass, e sessionstore.Entry) string {
	mentions := make([]string, 0, len(mentionIDs))
	for _, id := range mentionIDs {
		mentions = append(mentions, "<@"+id+">")
	}
	head := strings.Join(mentions, " ")
	if len(delivered) == 0 {
		return clampDiscord(head)
	}
	suffix := cmp.Or(strings.TrimSpace(e.CaseKey), strings.TrimSpace(e.Project))
	msg := head
	if msg != "" {
		msg += " — "
	}
	msg += slaClockPhrase(delivered[0].name, delivered[0].clock)
	if suffix != "" {
		msg += " · " + suffix
	}
	for _, c := range delivered[1:] {
		msg += "\n" + slaClockPhrase(c.name, c.clock)
	}
	return clampDiscord(msg)
}
