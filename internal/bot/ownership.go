package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// canControlThread reports whether the author may cancel/reset this thread.
// Soft policy: unowned sessions (legacy / not yet set) allow anyone on the allowlist.
// Owned sessions require owner, co-owner, or a project admin (a team whose
// capability template grants adminProject).
func (b *Bot) canControlThread(m *discordgo.MessageCreate, e sessionstore.Entry) bool {
	if m == nil || m.Author == nil {
		return false
	}
	return b.canControlUser(b.authorActorID(m), e)
}

func (b *Bot) denyControl(s *discordgo.Session, m *discordgo.MessageCreate, e sessionstore.Entry, action string) {
	msg := denyControlText(e, action)
	if _, err := discordReply(s, m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply deny-%s: %v", action, err)
	}
}

func (b *Bot) handleClaim(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := discordReply(s, m.ChannelID, "Use `@Grok /claim` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply claim-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}
	// Ownership is an account, not a login: resolved once here, then compared and
	// stored as-is. The <@…> mentions below deliberately keep m.Author.ID — those
	// address the Discord user who typed the command, not the account.
	authorID := b.authorActorID(m)

	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		// No session yet: create ownership shell so cancel/reset can bind later.
		// Project/cwd filled on the first real task.
		parentID := parentChannelID(s, m.ChannelID)
		projName := ""
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
		e = sessionstore.Entry{Project: projName}
		e.SetOwner(authorID, m.Author.String())
		err := b.sessions.Set(m.ChannelID, e)
		b.auditCmdMsg(audit.ActionSessionClaim, m, projName, err, map[string]any{"shell": true})
		if err != nil {
			log.Printf("error: claim save thread=%s: %v", m.ChannelID, err)
			if _, sendErr := discordReply(s, m.ChannelID, "Could not save ownership: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply claim-save: %v", sendErr)
			}
			return
		}
		if _, err := discordReply(s, m.ChannelID,
			fmt.Sprintf("You own this thread now (<@%s>). Cancel/reset are restricted to you and project admins.", m.Author.ID),
			ref(m)); err != nil {
			log.Printf("error: reply claim-new: %v", err)
		}
		return
	}

	if e.IsOwner(authorID) {
		if _, err := discordReply(s, m.ChannelID, "You already own this thread.", ref(m)); err != nil {
			log.Printf("error: reply claim-already: %v", err)
		}
		return
	}

	prevID, prevName := e.OwnerID, e.OwnerName
	// Claim is a full takeover: reset co-owners, then keep only the previous primary
	// so the list does not grow unbounded across repeated claims.
	e.CoOwnerIDs = nil
	e.SetOwner(authorID, m.Author.String())
	if prevID != "" && prevID != authorID {
		e.AddCoOwner(prevID)
	}
	err := b.sessions.Set(m.ChannelID, e)
	b.auditCmdMsg(audit.ActionSessionClaim, m, e.Project, err, map[string]any{
		"previousOwnerId": prevID,
		"takeover":        prevID != "",
	})
	if err != nil {
		log.Printf("error: claim save thread=%s: %v", m.ChannelID, err)
		if _, sendErr := discordReply(s, m.ChannelID, "Could not save ownership: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply claim-save: %v", sendErr)
		}
		return
	}

	msg := fmt.Sprintf("You claimed this thread (<@%s>). Cancel/reset are restricted to you, co-owners, and project admins.", m.Author.ID)
	if prevID != "" {
		label := prevName
		if label == "" {
			label = prevID
		}
		msg = fmt.Sprintf(
			"You claimed this thread from **%s** (<@%s>). <@%s> is primary owner; previous owner remains a co-owner.",
			label, prevID, m.Author.ID,
		)
	}
	if _, err := discordReply(s, m.ChannelID, msg, ref(m)); err != nil {
		log.Printf("error: reply claim: %v", err)
	}
}

func (b *Bot) handleHandOff(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := discordReply(s, m.ChannelID, "Use `@Grok /hand-off @user` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply handoff-not-thread: %v", err)
		}
		return
	}
	if m.Author == nil {
		return
	}

	// Both sides resolved to accounts: handing off to your own second login is
	// still handing off to yourself, and the new owner id has to be the account
	// so they keep control whichever way they log in next.
	authorID := b.authorActorID(m)
	target := firstMentionedUser(s, m)
	if target == nil {
		if _, err := discordReply(s, m.ChannelID, "Mention who should take ownership: `@Grok /hand-off @user`.", ref(m)); err != nil {
			log.Printf("error: reply handoff-need-user: %v", err)
		}
		return
	}
	targetID := b.userActorID(target)
	if targetID == authorID {
		if _, err := discordReply(s, m.ChannelID, "You already have the thread — no hand-off needed.", ref(m)); err != nil {
			log.Printf("error: reply handoff-self: %v", err)
		}
		return
	}

	e, ok := b.sessions.Get(m.ChannelID)
	if !ok {
		// Create shell with target as owner (author becomes co-owner via HandOff).
		parentID := parentChannelID(s, m.ChannelID)
		projName := ""
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
		e = sessionstore.Entry{Project: projName}
		e.SetOwner(authorID, m.Author.String())
		e.HandOff(targetID, target.String())
		err := b.sessions.Set(m.ChannelID, e)
		b.auditCmdMsg(audit.ActionSessionHandOff, m, projName, err, map[string]any{
			"toId":  targetID,
			"shell": true,
		})
		if err != nil {
			log.Printf("error: handoff save thread=%s: %v", m.ChannelID, err)
			if _, sendErr := discordReply(s, m.ChannelID, "Could not save ownership: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply handoff-save: %v", sendErr)
			}
			return
		}
		card := b.formatHandOffCard(m.ChannelID, e, m.Author, target)
		if _, err := discordReply(s, m.ChannelID, card, ref(m)); err != nil {
			log.Printf("error: reply handoff-card: %v", err)
		}
		if _, err := b.refreshBriefCard(s, m.ChannelID, e.Cwd); err != nil {
			log.Printf("brief: handoff refresh thread=%s: %v", m.ChannelID, err)
		}
		return
	}

	if !b.canControlUser(authorID, e) {
		b.auditCmdMsg(audit.ActionSessionHandOff, m, e.Project, errAuditDeniedControl, map[string]any{"toId": targetID})
		b.denyControl(s, m, e, "hand off this thread")
		return
	}
	// Unowned: claim as author first so HandOff records them as co-owner.
	if !e.HasOwner() {
		e.SetOwner(authorID, m.Author.String())
	}

	e.HandOff(targetID, target.String())
	err := b.sessions.Set(m.ChannelID, e)
	b.auditCmdMsg(audit.ActionSessionHandOff, m, e.Project, err, map[string]any{"toId": targetID})
	if err != nil {
		log.Printf("error: handoff save thread=%s: %v", m.ChannelID, err)
		if _, sendErr := discordReply(s, m.ChannelID, "Could not save ownership: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply handoff-save: %v", sendErr)
		}
		return
	}

	// Reload for card (co-owners etc.).
	if fresh, ok := b.sessions.Get(m.ChannelID); ok {
		e = fresh
	}
	card := b.formatHandOffCard(m.ChannelID, e, m.Author, target)
	if _, err := discordReply(s, m.ChannelID, card, ref(m)); err != nil {
		log.Printf("error: reply handoff-card: %v", err)
	}
	if _, err := b.refreshBriefCard(s, m.ChannelID, e.Cwd); err != nil {
		log.Printf("brief: handoff refresh thread=%s: %v", m.ChannelID, err)
	}
}

func firstMentionedUser(s *discordgo.Session, m *discordgo.MessageCreate) *discordgo.User {
	if m == nil {
		return nil
	}
	botID := ""
	if s != nil && s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	for _, u := range m.Mentions {
		if u == nil || u.ID == "" || u.ID == botID || u.Bot {
			continue
		}
		return u
	}
	return nil
}

func (b *Bot) formatHandOffCard(threadID string, e sessionstore.Entry, from, to *discordgo.User) string {
	fromLabel := "someone"
	toLabel := "someone"
	if from != nil {
		fromLabel = fmt.Sprintf("<@%s>", from.ID)
	}
	if to != nil {
		toLabel = fmt.Sprintf("<@%s>", to.ID)
	}

	state := "idle"
	if job, busy := b.getJob(threadID); busy {
		state = "running · " + formatElapsed(time.Since(job.start))
	}
	q := b.queueLen(threadID)
	if q > 0 {
		state += fmt.Sprintf(" · %d queued", q)
	}

	lines := []string{
		fmt.Sprintf("**Hand-off** · %s → %s", fromLabel, toLabel),
	}
	if e.Project != "" {
		lines = append(lines, "**project:** "+e.Project)
	}

	goal := strings.TrimSpace(e.Goal)
	if goal == "" {
		goal = b.lastPromptPreview(threadID)
	}
	if goal != "" {
		lines = append(lines, "**goal:** "+goal)
	}
	lab := sessionstore.DisplayLabel(e.EffectiveLabel())
	if e.LabelManual {
		lines = append(lines, "**label:** "+lab+" (manual)")
	} else {
		lines = append(lines, "**label:** "+lab)
	}
	lines = append(lines, "**status:** "+state)

	if e.WorktreeBranch != "" {
		lines = append(lines, "**worktree:** `"+e.WorktreeBranch+"`")
	}
	if issLines := sessionstore.FormatIssueStatusLines(e.Issues); len(issLines) > 0 {
		lines = append(lines, issLines...)
	}
	e.NormalizePRs()
	if prLines := ghpr.FormatMultiStatusLines(b.discordPRInfos(e)); len(prLines) > 0 {
		lines = append(lines, prLines...)
	} else {
		lines = append(lines, "**pr:** (none yet)")
	}
	if q > 0 {
		lines = append(lines, fmt.Sprintf("**queue:** %d pending", q))
	}
	if to != nil {
		lines = append(lines, fmt.Sprintf(
			"%s owns cancel/reset for this thread. Use `@Grok /claim` to take over later if needed.",
			toLabel,
		))
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) lastPromptPreview(threadID string) string {
	if b.history == nil || threadID == "" {
		return ""
	}
	th, err := b.history.Get(threadID)
	if err != nil || len(th.Turns) == 0 {
		return ""
	}
	p := strings.TrimSpace(th.Turns[len(th.Turns)-1].Prompt)
	if p == "" {
		return ""
	}
	// Strip remote-work prefix noise if somehow stored (user prompts are raw).
	const max = 160
	if len(p) <= max {
		return p
	}
	cut := strings.LastIndex(p[:max-1], " ")
	if cut < max/3 {
		cut = max - 1
	}
	return strings.TrimSpace(p[:cut]) + "…"
}

// ensureSessionOwner sets owner on first use when missing (first @Grok author).
func ensureSessionOwner(e *sessionstore.Entry, userID, displayName string) {
	if e == nil || userID == "" || e.HasOwner() {
		return
	}
	e.SetOwner(userID, displayName)
}

// bindThreadOwner persists owner early (start of a run) so cancel is gated before the
// first task finishes. No-op when an owner is already set. Preserves other session fields.
func (b *Bot) bindThreadOwner(threadID, project string, m *discordgo.MessageCreate) {
	if m == nil {
		return
	}
	b.bindThreadOwnerActor(threadID, project, b.actorFromUser(m.Author))
}

// bindThreadOwnerActor is the Discord-optional owner bind path.
func (b *Bot) bindThreadOwnerActor(threadID, project string, actor Actor) {
	if b == nil || b.sessions == nil || threadID == "" || actor.ID == "" {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if ok && e.HasOwner() {
		return
	}
	if !ok {
		e = sessionstore.Entry{}
	}
	if e.Project == "" {
		e.Project = project
	}
	ensureSessionOwner(&e, actor.ID, actor.String())
	if err := b.sessions.Set(threadID, e); err != nil {
		log.Printf("warn: bind owner thread=%s: %v", threadID, err)
	}
}

// preserveOwnershipFields copies ownership onto next when session Set overwrites the whole entry.
func preserveOwnershipFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.OwnerID == "" {
		next.OwnerID = prev.OwnerID
		next.OwnerName = prev.OwnerName
	}
	if len(next.CoOwnerIDs) == 0 && len(prev.CoOwnerIDs) > 0 {
		next.CoOwnerIDs = append([]string(nil), prev.CoOwnerIDs...)
	}
}
