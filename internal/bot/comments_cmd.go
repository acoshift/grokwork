package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/ghpr"
)

// handleComments: @Grok /comments — list unresolved PR review threads for this unit.
func (b *Bot) handleComments(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /comments` inside a Grok thread with a PR.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	e.NormalizePRs()
	pr, ok := e.PrimaryPR()
	if !ok || pr.Number <= 0 {
		replyText(s, m, "No tracked PR on this thread.")
		return
	}
	pr.FillOwnerRepoFromURL()
	cwd := e.Cwd
	if cwd == "" {
		cwd = e.MainCwd
	}
	if cwd == "" && b.cfg != nil {
		cwd, _ = b.cfg.ProjectPath(e.Project)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	comments, err := ghpr.ListUnresolvedReviewComments(ctx, cwd, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		replyText(s, m, "Could not list review comments: "+err.Error())
		return
	}
	if len(comments) == 0 {
		replyText(s, m, fmt.Sprintf("No unresolved review comments on %s/%s#%d.", pr.Owner, pr.Repo, pr.Number))
		return
	}
	var bld strings.Builder
	fmt.Fprintf(&bld, "**Unresolved review comments** on %s/%s#%d (%d):\n", pr.Owner, pr.Repo, pr.Number, len(comments))
	max := 12
	for i, c := range comments {
		if i >= max {
			fmt.Fprintf(&bld, "…and %d more. Use `@Grok /address` to fix them.\n", len(comments)-max)
			break
		}
		loc := c.Path
		if c.Line > 0 {
			loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
		}
		body := strings.TrimSpace(c.Body)
		if len(body) > 200 {
			body = body[:197] + "…"
		}
		author := c.Author
		if author == "" {
			author = "?"
		}
		fmt.Fprintf(&bld, "%d. **%s** · `%s`\n> %s\n", i+1, author, loc, body)
	}
	bld.WriteString("\n_Address with `@Grok /address`_")
	replyText(s, m, bld.String())
}

// handleAddress: @Grok /address — queue StartAddressReview on this thread's PR.
func (b *Bot) handleAddress(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /address` inside a Grok thread with a PR.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		replyText(s, m, "No session for this thread yet.")
		return
	}
	if !b.actorCanShip(m, e.Project) && !b.canControlThread(m, e) {
		replyText(s, m, "You're not allowed to `/address` (need builder caps or thread control).")
		return
	}
	e.NormalizePRs()
	pr, ok := e.PrimaryPR()
	if !ok || pr.Number <= 0 {
		replyText(s, m, "No tracked PR on this thread.")
		return
	}
	pr.FillOwnerRepoFromURL()
	cwd := e.Cwd
	if cwd == "" {
		cwd = e.MainCwd
	}
	if cwd == "" && b.cfg != nil {
		cwd, _ = b.cfg.ProjectPath(e.Project)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	comments, err := ghpr.ListUnresolvedReviewComments(ctx, cwd, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		replyText(s, m, "Could not list review comments: "+err.Error())
		return
	}
	// Same two sources as the web button, or `/address` and "Address review"
	// would mean different things under one name. Best-effort: unresolved
	// threads alone are still enough to run.
	detail, _ := ghpr.ViewPRDetail(ctx, cwd, pr.Selector())
	if len(comments) == 0 && len(detail.Comments) == 0 {
		replyText(s, m, "No unresolved review comments or PR comments to address.")
		return
	}
	actor := ActorFromUser(m.Author)
	res, err := b.StartAddressReview(AddressReviewOpts{
		Project:      e.Project,
		Actor:        actor,
		ThreadID:     m.ChannelID,
		Owner:        pr.Owner,
		Repo:         pr.Repo,
		Number:       pr.Number,
		Title:        pr.Title,
		URL:          pr.URL,
		Comments:     comments,
		Conversation: detail.Comments,
	})
	if err != nil {
		replyText(s, m, "Address failed: "+err.Error())
		return
	}
	msg := fmt.Sprintf("Queued **address review** for %s/%s#%d (%d review comments, %d PR comments).",
		pr.Owner, pr.Repo, pr.Number, len(comments), len(detail.Comments))
	if res.QueuePos > 0 {
		msg += fmt.Sprintf(" Queue position **%d**.", res.QueuePos)
	}
	replyText(s, m, msg)
}
