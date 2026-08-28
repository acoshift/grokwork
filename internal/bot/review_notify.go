package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

// teamReviewMention is the Discord snowflake to <@ping> when a live Discord
// thread exists. Empty means no thread ping (inbox still happens separately).
func teamReviewMention(hasDiscord bool, reviewerID, discordSubject string) string {
	if !hasDiscord {
		return ""
	}
	if sub := strings.TrimSpace(discordSubject); sub != "" {
		return sub
	}
	if config.IsDiscordActor(reviewerID) {
		return config.ActorSubject(reviewerID)
	}
	return ""
}

func formatTeamReviewMention(snowflake, owner, repo string, number int, note, prURL string) string {
	msg := fmt.Sprintf("<@%s> please review **%s/%s#%d**", snowflake, owner, repo, number)
	if note != "" {
		msg += "\n> " + note
	}
	if prURL != "" {
		msg += "\n" + prURL
	}
	return msg
}

func (b *Bot) queueReviewInbox(req reviewstore.Request) {
	if b == nil {
		return
	}
	if err := b.QueueInbox(req.ReviewerID, inbox.KindReviewRequested,
		fmt.Sprintf("Review requested · %s/%s#%d", req.Owner, req.Repo, req.Number),
		req.Note, inboxPRPath(req.Owner, req.Repo, req.Number, req.Project),
		req.ThreadID, req.Project); err != nil {
		log.Printf("warn: inbox review request reviewer=%s: %v", req.ReviewerID, err)
	}
}

// NotifyTeamReviewRequested pings the reviewer like the web review button:
// always queue inbox, and mention on a live Discord thread when a snowflake exists.
func (b *Bot) NotifyTeamReviewRequested(req reviewstore.Request) {
	if b == nil {
		return
	}
	b.queueReviewInbox(req)
	sub, ok := "", false
	if id := b.Identity(); id != nil {
		sub, ok = id.DiscordSubjectFor(req.ReviewerID)
	}
	if !ok {
		sub = ""
	}
	snowflake := teamReviewMention(b.hasDiscordSurface(req.ThreadID), req.ReviewerID, sub)
	if snowflake == "" {
		return
	}
	prURL := ""
	if b.cfg != nil {
		prURL = b.cfg.DiscordPRDisplayURL(req.Owner, req.Repo, req.Number, "")
	}
	go b.NotifyThread(req.ThreadID, formatTeamReviewMention(snowflake, req.Owner, req.Repo, req.Number, req.Note, prURL))
}
