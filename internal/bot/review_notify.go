package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

// teamReviewNotify decides how a team-review request reaches the reviewer.
// mentionSnowflake is the Discord subject to <@ping> when a live Discord thread exists.
// inbox is true when that mention cannot be sent.
func teamReviewNotify(threadID, reviewerID, discordSubject string, hasDiscordSubject bool) (mentionSnowflake string, inbox bool) {
	snowflake := ""
	if hasDiscordSubject {
		snowflake = strings.TrimSpace(discordSubject)
	}
	if snowflake == "" && config.IsDiscordActor(reviewerID) {
		snowflake = config.ActorSubject(reviewerID)
	}
	if strings.TrimSpace(threadID) != "" && !gitworktree.IsWebUnitID(threadID) && snowflake != "" {
		return snowflake, false
	}
	return "", true
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

// NotifyTeamReviewRequested pings the reviewer like the web review button:
// Discord thread mention when a snowflake is reachable, otherwise inbox.
func (b *Bot) NotifyTeamReviewRequested(req reviewstore.Request) {
	if b == nil {
		return
	}
	sub, ok := "", false
	if id := b.Identity(); id != nil {
		sub, ok = id.DiscordSubjectFor(req.ReviewerID)
	}
	snowflake, needInbox := teamReviewNotify(req.ThreadID, req.ReviewerID, sub, ok)
	prURL := ""
	if b.cfg != nil {
		prURL = b.cfg.DiscordPRDisplayURL(req.Owner, req.Repo, req.Number, "")
	}
	if snowflake != "" {
		// Best-effort after the request is durable — same as the web handler.
		go b.NotifyThread(req.ThreadID, formatTeamReviewMention(snowflake, req.Owner, req.Repo, req.Number, req.Note, prURL))
	}
	if !needInbox {
		return
	}
	if err := b.QueueInbox(req.ReviewerID, "review.requested",
		fmt.Sprintf("Review requested · %s/%s#%d", req.Owner, req.Repo, req.Number),
		req.Note, prURL, req.ThreadID, req.Project); err != nil {
		log.Printf("warn: inbox review request reviewer=%s: %v", req.ReviewerID, err)
	}
}
