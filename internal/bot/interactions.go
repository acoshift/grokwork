package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

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
	action, threadID, ok := parseActionCustomID(data.CustomID)
	if !ok {
		return
	}

	user := interactionUser(i)
	if user == nil {
		respondEphemeral(s, i, "Could not resolve your Discord user.")
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
	if project == "" || !b.isAllowedUser(s, i.GuildID, user.ID, project, i.Member) {
		msg := "You're not allowed to use Grok on this project."
		if project != "" {
			msg = fmt.Sprintf("You're not allowed to use Grok on project **%s**.", project)
		}
		respondEphemeral(s, i, msg)
		return
	}

	switch action {
	case actionCancel:
		b.interactionCancel(s, i, threadID, user)
	case actionContinue:
		if err := s.InteractionRespond(i.Interaction, continueModal(threadID)); err != nil {
			log.Printf("error: continue modal thread=%s: %v", threadID, err)
		}
	case actionReset:
		b.interactionResetPrompt(s, i, threadID, user)
	case actionResetOK:
		b.interactionResetConfirm(s, i, threadID, user)
	case actionResetNo:
		respondEphemeral(s, i, "Reset cancelled.")
	case actionHistory:
		respondEphemeral(s, i, historyHint(threadID, b.cfg.ListenAddr()))
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
	if project == "" || !b.isAllowedUser(s, i.GuildID, user.ID, project, i.Member) {
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
	go b.handleTask(s, m, Parsed{Kind: KindTask, Prompt: prompt})
}

func (b *Bot) interactionCancel(s *discordgo.Session, i *discordgo.InteractionCreate, threadID string, user *discordgo.User) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(s, threadID, user.ID, e) {
		respondEphemeral(s, i, denyControlText(e, "cancel"))
		return
	}
	msg, ok := b.cancelCurrentRun(threadID, user.String())
	if !ok {
		respondEphemeral(s, i, msg)
		return
	}
	// Ack privately + announce in thread (matches /cancel visibility).
	respondEphemeral(s, i, msg)
	if _, err := discordSend(s, threadID, msg+" (via button · <@"+user.ID+">)"); err != nil {
		log.Printf("error: cancel announce thread=%s: %v", threadID, err)
	}
}

func (b *Bot) interactionResetPrompt(s *discordgo.Session, i *discordgo.InteractionCreate, threadID string, user *discordgo.User) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(s, threadID, user.ID, e) {
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

func (b *Bot) interactionResetConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, threadID string, user *discordgo.User) {
	if e, ok := b.sessions.Get(threadID); ok && !b.canControlUser(s, threadID, user.ID, e) {
		respondEphemeral(s, i, denyControlText(e, "reset"))
		return
	}
	msg, err := b.resetThreadCore(threadID)
	if err != nil {
		respondEphemeral(s, i, msg)
		return
	}
	respondEphemeral(s, i, msg)
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

func denyControlText(e sessionstore.Entry, action string) string {
	owner := e.OwnerName
	if owner == "" {
		owner = e.OwnerID
	}
	if owner != "" && owner != e.OwnerID {
		return fmt.Sprintf(
			"Only the thread owner (**%s** / <@%s>), co-owners, or a Discord mod can %s. Ask them, or `@Grok /claim` to take ownership.",
			owner, e.OwnerID, action,
		)
	}
	return fmt.Sprintf(
		"Only the thread owner (<@%s>), co-owners, or a Discord mod can %s. Ask them, or `@Grok /claim` to take ownership.",
		e.OwnerID, action,
	)
}

// isAllowedUser checks project membership for Discord users.
func (b *Bot) isAllowedUser(s *discordgo.Session, guildID, userID, project string, member *discordgo.Member) bool {
	if b == nil || b.cfg == nil || userID == "" || project == "" {
		return false
	}
	if !b.cfg.ProjectHasAllowlist(project) {
		return false
	}
	if b.cfg.AccessAllowed(project, userID, nil) {
		return true
	}
	if member == nil && s != nil && guildID != "" {
		var err error
		member, err = s.GuildMember(guildID, userID)
		if err != nil {
			return false
		}
	}
	if member == nil {
		return false
	}
	return b.cfg.AccessAllowed(project, userID, member.Roles)
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
func (b *Bot) canControlUser(s *discordgo.Session, channelID, userID string, e sessionstore.Entry) bool {
	if userID == "" {
		return false
	}
	if !e.HasOwner() {
		return true
	}
	if e.CanControl(userID) {
		return true
	}
	return b.isModeratorUser(s, channelID, userID)
}

func (b *Bot) isModeratorUser(s *discordgo.Session, channelID, userID string) bool {
	if s == nil || channelID == "" || userID == "" {
		return false
	}
	perms, err := s.UserChannelPermissions(userID, channelID)
	if err != nil {
		log.Printf("warn: UserChannelPermissions user=%s channel=%s: %v", userID, channelID, err)
		return false
	}
	const modBits = discordgo.PermissionAdministrator |
		discordgo.PermissionManageMessages |
		discordgo.PermissionManageThreads
	return perms&modBits != 0
}
