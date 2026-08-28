package bot

import (
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/inbox"
)

// Inbox returns the per-actor notification feed (may be nil if init failed).
func (b *Bot) Inbox() *inbox.Store {
	if b == nil {
		return nil
	}
	return b.inbox
}

// Delivery routes describe how one notification reaches its recipients.
//
// The shape that matters is the *set*: a shared channel takes every recipient at
// once and posts a single message naming all of them, while a per-actor channel
// is called once each. Collapsing those into one per-actor interface is what
// turns a thread's single "@you @them — run done" into three separate DMs, which
// is a regression nobody notices in review and everybody notices in Discord.
//
//	thread  → one message, all recipients        (shared)
//	dm      → one message per recipient          (per actor)
//	inbox   → one entry per recipient            (per actor, always reachable)
//
// Ordering is: inbox is written for every recipient first (the durable record),
// then a live thread takes the whole set as one message; otherwise each
// recipient is DMed if Discord can reach them. A failed or capped Discord send
// must not mean the inbox row is missing.

// canDM reports whether a recipient id can receive a Discord DM. Only a Discord
// actor can: a web-only login has no DM channel to open.
func canDM(actorID string) bool {
	return looksLikeDiscordUserID(actorID)
}

// inboxSessionPath is the in-app link stored on a run.done row. It is
// root-relative so it works without webPublicBaseURL (sessionWebURL returns
// empty when that is unset).
func inboxSessionPath(threadID, project string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	u := "/sessions/" + url.PathEscape(threadID)
	if p := strings.TrimSpace(project); p != "" {
		u += "?project=" + url.QueryEscape(p)
	}
	return u
}

// inboxPRPath is the in-app link stored on a review.requested row.
func inboxPRPath(owner, repo string, number int, project string) string {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" || number <= 0 {
		return ""
	}
	u := "/prs/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + strconv.Itoa(number)
	if p := strings.TrimSpace(project); p != "" {
		u += "?project=" + url.QueryEscape(p)
	}
	return u
}

// deliverInbox queues a run-finished notification for each recipient. Per actor,
// since an inbox entry addressed to one person must not name the others.
func (b *Bot) deliverInbox(actorIDs []string, unitID string, outcome runOutcome, elapsed time.Duration) {
	if b == nil || b.inbox == nil || len(actorIDs) == 0 {
		return
	}
	subject := "Run " + outcomeLabel(outcome) + " · " + formatElapsed(elapsed)
	project, goal := "", ""
	if b.sessions != nil {
		if e, ok := b.sessions.Get(unitID); ok {
			project = strings.TrimSpace(e.Project)
			goal = strings.TrimSpace(e.Goal)
		}
	}
	if project != "" {
		subject += " · " + project
	}
	item := inbox.Item{
		Kind:    inbox.KindRunDone,
		Subject: subject,
		Body:    truncateRunes(goal, 200),
		URL:     inboxSessionPath(unitID, project),
		UnitID:  unitID,
		Project: project,
	}
	for _, id := range actorIDs {
		if _, err := b.inbox.Append(id, item); err != nil {
			// One bad recipient must not silence the rest.
			log.Printf("warn: inbox append actor=%s unit=%s: %v", id, unitID, err)
		}
	}
}

// QueueInbox delivers an arbitrary notification to one actor. Used by surfaces
// that have no push channel for a recipient (web-only members).
func (b *Bot) QueueInbox(actorID, kind, subject, body, url, unitID, project string) error {
	if b == nil || b.inbox == nil {
		return nil
	}
	_, err := b.inbox.Append(actorID, inbox.Item{
		Kind:    kind,
		Subject: subject,
		Body:    body,
		URL:     url,
		UnitID:  unitID,
		Project: project,
	})
	return err
}
