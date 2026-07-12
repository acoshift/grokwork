package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/grokrun"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

const maxMsg = 1900

type Bot struct {
	cfg      *config.Config
	sessions *sessionstore.Store
	busy     sync.Map // threadID → struct{}
}

func New(cfg *config.Config, sessions *sessionstore.Store) *Bot {
	return &Bot{cfg: cfg, sessions: sessions}
}

func (b *Bot) Register(s *discordgo.Session) {
	s.AddHandler(b.onReady)
	s.AddHandler(b.onMessage)
	// Message Content is privileged — enable it in Developer Portal → Bot →
	// Privileged Gateway Intents. Do not request GuildMembers (also privileged);
	// role checks use m.Member from message events when present.
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("Logged in as %s", r.User.String())
	names := make([]string, 0, len(b.cfg.Projects))
	for n := range b.cfg.Projects {
		names = append(names, n)
	}
	log.Printf("Projects: %s", strings.Join(names, ", "))
	_ = s.UpdateGameStatus(0, "@Grok <task>")
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return
	}
	if !mentionsUser(m, s.State.User.ID) {
		return
	}

	if !b.isAllowed(s, m) {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "You're not on the allowlist for this Grok bridge.", ref(m))
		return
	}

	parsed := ParseMessage(m.Content, s.State.User.ID)

	switch parsed.Kind {
	case KindEmpty, KindHelp:
		_, _ = s.ChannelMessageSendReply(m.ChannelID, HelpText(), ref(m))
	case KindProjects:
		parentID := parentChannelID(s, m.ChannelID)
		msg := b.channelProjectHelp(parentID)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, msg, ref(m))
	case KindReset:
		if !isThread(s, m.ChannelID) {
			_, _ = s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /reset` inside a Grok thread.", ref(m))
			return
		}
		_ = b.sessions.Delete(m.ChannelID)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Session cleared for this thread.", ref(m))
	case KindStatus:
		if !isThread(s, m.ChannelID) {
			_, _ = s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /status` inside a Grok thread.", ref(m))
			return
		}
		e, ok := b.sessions.Get(m.ChannelID)
		if !ok {
			_, _ = s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m))
			return
		}
		state := "idle"
		if _, busy := b.busy.Load(m.ChannelID); busy {
			state = "running"
		}
		_, _ = s.ChannelMessageSendReply(m.ChannelID, strings.Join([]string{
			"**project:** " + e.Project,
			"**cwd:** `" + e.Cwd + "`",
			"**session:** `" + e.SessionID + "`",
			"**updated:** " + e.UpdatedAt,
			"**state:** " + state,
		}, "\n"), ref(m))
	case KindTask:
		go b.handleTask(s, m, parsed)
	}
}

func (b *Bot) isAllowed(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if len(b.cfg.AllowedUsers) == 0 && len(b.cfg.AllowedRoles) == 0 {
		return false
	}
	if _, ok := b.cfg.AllowedUsers[m.Author.ID]; ok {
		return true
	}
	if len(b.cfg.AllowedRoles) == 0 {
		return false
	}
	member := m.Member
	if member == nil {
		var err error
		member, err = s.GuildMember(m.GuildID, m.Author.ID)
		if err != nil {
			return false
		}
	}
	for _, roleID := range member.Roles {
		if _, ok := b.cfg.AllowedRoles[roleID]; ok {
			return true
		}
	}
	return false
}

type projectRef struct {
	Name string
	Cwd  string
}

// resolveProject maps the Discord parent channel → project via config.channels only.
func (b *Bot) resolveProject(channelID string) (projectRef, error) {
	mapped, ok := b.cfg.Channels[channelID]
	if !ok || mapped == "" {
		return projectRef{}, fmt.Errorf("this channel is not mapped to a project (admin: set `channels.%s` in config)", channelID)
	}
	cwd, ok := b.cfg.Projects[mapped]
	if !ok || cwd == "" {
		return projectRef{}, fmt.Errorf("channel maps to project `%s`, but that project is missing from config.projects", mapped)
	}
	return projectRef{Name: mapped, Cwd: cwd}, nil
}

func (b *Bot) channelProjectHelp(channelID string) string {
	proj, err := b.resolveProject(channelID)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("This channel → **%s** (`%s`)", proj.Name, proj.Cwd)
}

func parentChannelID(s *discordgo.Session, channelID string) string {
	if !isThread(s, channelID) {
		return channelID
	}
	ch, err := s.Channel(channelID)
	if err == nil && ch.ParentID != "" {
		return ch.ParentID
	}
	return channelID
}

func (b *Bot) handleTask(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	proj, err := b.resolveProject(parentChannelID(s, m.ChannelID))
	if err != nil {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, err.Error(), ref(m))
		return
	}

	threadID, err := b.ensureThread(s, m)
	if err != nil {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "Could not open thread: "+err.Error(), ref(m))
		return
	}

	if _, loaded := b.busy.LoadOrStore(threadID, struct{}{}); loaded {
		_, _ = s.ChannelMessageSend(threadID, "Already working in this thread — wait for the current run to finish.")
		return
	}
	defer b.busy.Delete(threadID)

	status, err := s.ChannelMessageSend(threadID, fmt.Sprintf("Working in **%s** (`%s`)…", proj.Name, proj.Cwd))
	if err != nil {
		log.Printf("status message: %v", err)
		return
	}

	var sessionID string
	if e, ok := b.sessions.Get(threadID); ok {
		sessionID = e.SessionID
	}

	result := grokrun.Run(context.Background(), grokrun.Options{
		GrokBin:   b.cfg.GrokBin,
		Prompt:    parsed.Prompt,
		Cwd:       proj.Cwd,
		SessionID: sessionID,
		Yolo:      b.cfg.YoloEnabled(),
		Model:     b.cfg.Model,
		MaxTurns:  b.cfg.MaxTurns,
		Timeout:   time.Duration(b.cfg.TimeoutMs) * time.Millisecond,
		ExtraArgs: b.cfg.ExtraArgs,
	})

	if result.SessionID != "" {
		_ = b.sessions.Set(threadID, sessionstore.Entry{
			SessionID: result.SessionID,
			Project:   proj.Name,
			Cwd:       proj.Cwd,
			LastUser:  m.Author.String(),
		})
	}

	header := fmt.Sprintf("Done · **%s**", proj.Name)
	if result.Code != 0 {
		header = fmt.Sprintf("Finished with exit **%d** · **%s**", result.Code, proj.Name)
	}
	_, _ = s.ChannelMessageEdit(threadID, status.ID, header)

	sendChunks(s, threadID, result.Text)

	if result.Stderr != "" && os.Getenv("GROK_DISCORD_DEBUG") != "" {
		errText := result.Stderr
		if len(errText) > 1500 {
			errText = errText[:1500]
		}
		sendChunks(s, threadID, "stderr:\n```\n"+errText+"\n```")
	}
}

func (b *Bot) ensureThread(s *discordgo.Session, m *discordgo.MessageCreate) (string, error) {
	if isThread(s, m.ChannelID) {
		return m.ChannelID, nil
	}
	name := "grok: " + m.Author.Username
	if len(name) > 100 {
		name = name[:100]
	}
	th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

func mentionsUser(m *discordgo.MessageCreate, userID string) bool {
	for _, u := range m.Mentions {
		if u.ID == userID {
			return true
		}
	}
	// Fallback: content may still contain mention markup
	return strings.Contains(m.Content, "<@"+userID+">") || strings.Contains(m.Content, "<@!"+userID+">")
}

func isThread(s *discordgo.Session, channelID string) bool {
	ch, err := s.State.Channel(channelID)
	if err != nil {
		ch, err = s.Channel(channelID)
		if err != nil {
			return false
		}
	}
	return ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread ||
		ch.Type == discordgo.ChannelTypeGuildNewsThread
}

func ref(m *discordgo.MessageCreate) *discordgo.MessageReference {
	return &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}
}

func sendChunks(s *discordgo.Session, channelID, text string) {
	parts := splitMessage(text)
	for i, p := range parts {
		content := p
		if len(parts) > 1 {
			content = fmt.Sprintf("(%d/%d)\n%s", i+1, len(parts), p)
		}
		if _, err := s.ChannelMessageSend(channelID, content); err != nil {
			log.Printf("send chunk: %v", err)
		}
	}
}

func splitMessage(text string) []string {
	if text == "" {
		return []string{"(empty response)"}
	}
	if len(text) <= maxMsg {
		return []string{text}
	}
	var parts []string
	rest := text
	for len(rest) > maxMsg {
		cut := strings.LastIndex(rest[:maxMsg], "\n")
		if cut < maxMsg/2 {
			cut = maxMsg
		}
		parts = append(parts, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], "\n")
	}
	if rest != "" {
		parts = append(parts, rest)
	}
	return parts
}


