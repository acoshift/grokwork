package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// buttonAuditDetail marks a row as produced by a message-component click rather
// than a text command — the same mutation, a different affordance. One shared value
// is safe: auditCmd copies detail into a fresh map and never writes back.
var buttonAuditDetail = map[string]any{"via": "button"}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.Interaction == nil {
		return
	}
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		b.handleComponent(s, i)
	case discordgo.InteractionModalSubmit:
		b.handleModalSubmit(s, i)
	}
}

func (b *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()

	user := interactionUser(i)
	if user == nil {
		respondEphemeral(s, i, "Could not resolve your Discord user.")
		return
	}
	// A click mints an actor id exactly like a message does; resolve it once here
	// so every gate below asks about the account, not the login.
	actorID := b.userActorID(user)

	// Wave 2 decision buttons (gd:d:<thread>:<qid>:<idx>)
	if tid, qid, idx, ok := parseDecisionCustomID(data.CustomID); ok {
		if i.ChannelID != "" && i.ChannelID != tid {
			respondEphemeral(s, i, "This control belongs to another thread.")
			return
		}
		if !isThread(s, tid) {
			respondEphemeral(s, i, "Use these buttons inside a Grok thread.")
			return
		}
		project := b.projectForThread(s, tid)
		if project == "" || !b.isAllowedUser(actorID, project) {
			msg := "You're not allowed to use Grok on this project."
			if project != "" {
				msg = fmt.Sprintf("You're not allowed to use Grok on project **%s**.", project)
			}
			respondEphemeral(s, i, msg)
			return
		}
		b.handleDecisionClick(s, i, tid, qid, idx, user)
		return
	}

	action, threadID, ok := parseActionCustomID(data.CustomID)
	if !ok {
		return
	}

	// Buttons are scoped to a thread; ignore mismatches (stale message).
	if i.ChannelID != "" && i.ChannelID != threadID {
		respondEphemeral(s, i, "This control belongs to another thread.")
		return
	}
	if !isThread(s, threadID) {
		respondEphemeral(s, i, "Use these buttons inside a Grok thread.")
		return
	}

	project := b.projectForThread(s, threadID)
	if project == "" || !b.isAllowedUser(actorID, project) {
		msg := "You're not allowed to use Grok on this project."
		if project != "" {
			msg = fmt.Sprintf("You're not allowed to use Grok on project **%s**.", project)
		}
		respondEphemeral(s, i, msg)
		return
	}

	switch action {
	case actionCancel:
		b.interactionCancel(s, i, threadID, project, user)
	case actionContinue:
		if err := s.InteractionRespond(i.Interaction, continueModal(threadID)); err != nil {
			log.Printf("error: continue modal thread=%s: %v", threadID, err)
		}
	case actionReset:
		b.interactionResetPrompt(s, i, threadID, project, user)
	case actionResetOK:
		b.interactionResetConfirm(s, i, threadID, project, user)
	case actionResetNo:
		// Update the confirm prompt so Yes/Never mind cannot be re-clicked.
		respondUpdateMessage(s, i, "Reset cancelled.")
	case actionHistory:
		base := ""
		if b.cfg != nil {
			base = b.cfg.WebPublicBaseURLValue()
		}
		respondEphemeral(s, i, historyHint(threadID, base))
	default:
		respondEphemeral(s, i, "Unknown action.")
	}
}

func (b *Bot) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	action, threadID, ok := parseActionCustomID(data.CustomID)
	if !ok || action != actionContinueMod {
		return
	}

	user := interactionUser(i)
	if user == nil {
		respondEphemeral(s, i, "Could not resolve your Discord user.")
		return
	}
	if i.ChannelID != "" && i.ChannelID != threadID {
		respondEphemeral(s, i, "This control belongs to another thread.")
		return
	}
	if !isThread(s, threadID) {
		respondEphemeral(s, i, "Use Continue inside a Grok thread.")
		return
	}
	project := b.projectForThread(s, threadID)
	if project == "" || !b.isAllowedUser(b.userActorID(user), project) {
		msg := "You're not allowed to use Grok on this project."
		if project != "" {
			msg = fmt.Sprintf("You're not allowed to use Grok on project **%s**.", project)
		}
		respondEphemeral(s, i, msg)
		return
	}

	prompt := normalizeUserPrompt(modalTextValue(data, continueModalPromptID))
	if prompt == "" {
		respondEphemeral(s, i, "Follow-up was empty.")
		return
	}

	ack := "Starting follow-up…"
	if _, busy := b.getJob(threadID); busy {
		ack = "Queued follow-up…"
	}
	respondEphemeral(s, i, ack)

	// Public ack in thread so others see who continued (ephemeral is private).
	if _, err := discordSend(s, threadID, fmt.Sprintf("**Continue** from <@%s>:\n%s", user.ID, truncate(prompt, 500))); err != nil {
		log.Printf("error: continue announce thread=%s: %v", threadID, err)
	}

	m := messageCreateFromInteraction(i, user, prompt)
	// Audited by handleTaskOrigin (it owns the capability gate), tagged as a button
	// so a run started from a card is distinguishable from a typed follow-up.
	go b.handleTaskOrigin(s, m, Parsed{Kind: KindTask, Prompt: prompt}, auditOriginButton, buttonAuditDetail)
}

// interactionCancel is the completion card's Cancel button. It performs exactly
// what `@Grok /cancel` does behind the same ownership gate, so it writes the same
// audit rows — the button is the path most runs are actually cancelled from, and
// detail["via"] is what tells the two apart afterwards.
func (b *Bot) interactionCancel(
	s *discordgo.Session, i *discordgo.InteractionCreate, threadID, project string, user *discordgo.User,
) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(b.userActorID(user), e) {
		b.auditCmd(audit.ActionSessionCancel, b.actorFromUser(user), threadID, project,
			errAuditDeniedControl, buttonAuditDetail)
		respondEphemeral(s, i, denyControlText(e, "cancel"))
		return
	}
	msg, ok := b.cancelCurrentRun(threadID, user.String())
	if !ok {
		// Same distinction /cancel makes: "nothing was running" is not a denial.
		b.auditCmd(audit.ActionSessionCancel, b.actorFromUser(user), threadID, project,
			errors.New(msg), buttonAuditDetail)
		respondEphemeral(s, i, msg)
		return
	}
	b.auditCmd(audit.ActionSessionCancel, b.actorFromUser(user), threadID, project, nil, buttonAuditDetail)
	// Ack privately + announce in thread (matches /cancel visibility).
	respondEphemeral(s, i, msg)
	if _, err := discordSend(s, threadID, msg+" (via button · <@"+user.ID+">)"); err != nil {
		log.Printf("error: cancel announce thread=%s: %v", threadID, err)
	}
}

// interactionResetPrompt shows the confirm. Nothing is mutated here, but this is
// where a user without control is actually turned away (the confirm buttons are
// never rendered for them), so the denial has to be recorded here or the common
// refusal leaves no row.
func (b *Bot) interactionResetPrompt(
	s *discordgo.Session, i *discordgo.InteractionCreate, threadID, project string, user *discordgo.User,
) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(b.userActorID(user), e) {
		b.auditCmd(audit.ActionSessionReset, b.actorFromUser(user), threadID, project,
			errAuditDeniedControl, buttonAuditDetail)
		respondEphemeral(s, i, denyControlText(e, "reset"))
		return
	}
	if _, busy := b.getJob(threadID); busy {
		respondEphemeral(s, i, "A run is in progress — Cancel first, then Reset.")
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    "Reset this thread's session and worktree? This cannot be undone.",
			Flags:      discordgo.MessageFlagsEphemeral,
			Components: actionBarResetConfirm(threadID),
		},
	}); err != nil {
		log.Printf("error: reset confirm prompt thread=%s: %v", threadID, err)
	}
}

func (b *Bot) interactionResetConfirm(
	s *discordgo.Session, i *discordgo.InteractionCreate, threadID, project string, user *discordgo.User,
) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(b.userActorID(user), e) {
		// Reachable when control changed between the prompt and the click, so it is a
		// real refusal of a real attempt and gets its own row.
		b.auditCmd(audit.ActionSessionReset, b.actorFromUser(user), threadID, project,
			errAuditDeniedControl, buttonAuditDetail)
		// Replace the confirm prompt (drops Yes/Never mind).
		respondUpdateMessage(s, i, denyControlText(e, "reset"))
		return
	}
	msg, err := b.resetThreadCore(threadID)
	// Recorded before the error branch: a reset that dropped the worktree and then
	// failed halfway is exactly the row an operator needs.
	b.auditCmd(audit.ActionSessionReset, b.actorFromUser(user), threadID, project, err, buttonAuditDetail)
	if err != nil {
		respondUpdateMessage(s, i, msg)
		return
	}
	// Show result on the confirm message and strip buttons so Reset cannot re-fire.
	respondUpdateMessage(s, i, msg)
	if _, sendErr := discordSend(s, threadID, msg+" (via button · <@"+user.ID+">)"); sendErr != nil {
		log.Printf("error: reset announce thread=%s: %v", threadID, sendErr)
	}
}

func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i == nil {
		return nil
	}
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

func messageCreateFromInteraction(i *discordgo.InteractionCreate, user *discordgo.User, content string) *discordgo.MessageCreate {
	m := &discordgo.Message{
		ChannelID: i.ChannelID,
		GuildID:   i.GuildID,
		Author:    user,
		Member:    i.Member,
		Content:   content,
	}
	// Stable-ish id for attachment dirs / logs (not a real Discord message id).
	if i.ID != "" {
		m.ID = "ix:" + i.ID
	}
	return &discordgo.MessageCreate{Message: m}
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if s == nil || i == nil {
		return
	}
	content = sanitizeDiscordContent(content)
	if content == "" {
		content = "(empty)"
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("error: interaction respond: %v", err)
	}
}

// respondUpdateMessage edits the interaction's source message (e.g. an
// ephemeral confirm) and clears components so buttons cannot be re-clicked.
func respondUpdateMessage(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if s == nil || i == nil {
		return
	}
	content = sanitizeDiscordContent(content)
	if content == "" {
		content = "(empty)"
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: []discordgo.MessageComponent{},
		},
	}); err != nil {
		log.Printf("error: interaction update: %v", err)
	}
}

func denyControlText(e sessionstore.Entry, action string) string {
	owner := e.OwnerName
	if owner == "" {
		owner = e.OwnerID
	}
	if owner != "" && owner != e.OwnerID {
		return fmt.Sprintf(
			"Only the thread owner (**%s** / <@%s>), co-owners, or a project admin can %s. Ask them, or `@Grok /claim` to take ownership.",
			owner, e.OwnerID, action,
		)
	}
	return fmt.Sprintf(
		"Only the thread owner (<@%s>), co-owners, or a project admin can %s. Ask them, or `@Grok /claim` to take ownership.",
		e.OwnerID, action,
	)
}

// isAllowedUser checks project membership for a Discord user. Membership is
// per-project data (allowedUserIds + teams) — nothing about the guild is
// consulted, so no Discord round-trip is needed to answer it.
func (b *Bot) isAllowedUser(userID, project string) bool {
	if b == nil || b.cfg == nil || userID == "" || project == "" {
		return false
	}
	if !b.cfg.ProjectHasAllowlist(project) {
		return false
	}
	return b.cfg.AccessAllowed(project, userID)
}

// projectForThread resolves the project for a Discord thread (session, else parent channel map).
func (b *Bot) projectForThread(s *discordgo.Session, threadID string) string {
	if e, ok := b.sessions.Get(threadID); ok && strings.TrimSpace(e.Project) != "" {
		return e.Project
	}
	parent := parentChannelID(s, threadID)
	if name, ok := b.cfg.ChannelProject(parent); ok {
		return name
	}
	return ""
}

// canControlUser reports whether userID may cancel/reset this thread.
// Authority: owner, co-owner, or a project admin (a team whose capability
// template grants adminProject). Discord channel permissions are irrelevant —
// authorization is per-project, not per-guild.
func (b *Bot) canControlUser(userID string, e sessionstore.Entry) bool {
	if userID == "" {
		return false
	}
	if !e.HasOwner() {
		return true
	}
	if e.CanControl(userID) {
		return true
	}
	return b.actorAdminsProject(e.Project, userID)
}

// actorAdminsProject is the replacement for the old Discord moderator bypass.
//
// The AccessAllowed pre-check is load-bearing: with safeTeamMode on and
// safeTeamDefaultTemplate "admin", ResolveCapabilities would otherwise hand
// AdminProject to anyone at all. An empty project (a /claim shell with no
// project yet) resolves to false, leaving owner/co-owner only.
func (b *Bot) actorAdminsProject(project, userID string) bool {
	if b == nil || b.cfg == nil || project == "" || userID == "" {
		return false
	}
	if !b.cfg.AccessAllowed(project, userID) {
		return false
	}
	return b.cfg.ResolveCapabilities(project, userID).AdminProject
}
