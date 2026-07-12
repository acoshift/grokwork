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
	log.Printf("ready: logged in as %s (id=%s)", r.User.String(), r.User.ID)
	names := make([]string, 0, len(b.cfg.Projects))
	for n := range b.cfg.Projects {
		names = append(names, n)
	}
	log.Printf("ready: projects=%s channels=%d allowUsers=%d allowRoles=%d",
		strings.Join(names, ","), len(b.cfg.Channels), len(b.cfg.AllowedUsers), len(b.cfg.AllowedRoles))
	for ch, proj := range b.cfg.Channels {
		log.Printf("ready: channel %s → %s", ch, proj)
	}
	_ = s.UpdateGameStatus(0, "@Grok <task>")
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil {
		return
	}
	if m.Author.Bot {
		return
	}
	if m.GuildID == "" {
		return
	}
	if s.State.User == nil {
		log.Printf("error: message %s from %s but State.User is nil", m.ID, m.Author.ID)
		return
	}
	if !mentionsUser(m, s.State.User.ID) {
		return
	}

	log.Printf("msg: id=%s user=%s(%s) channel=%s guild=%s content=%q mentions=%d",
		m.ID, m.Author.String(), m.Author.ID, m.ChannelID, m.GuildID, truncate(m.Content, 500), len(m.Mentions))

	if m.Content == "" {
		log.Printf("warn: empty content on mention — enable Message Content Intent in Developer Portal")
	}

	if !b.isAllowed(s, m) {
		log.Printf("deny: user %s(%s) not on allowlist", m.Author.String(), m.Author.ID)
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "You're not on the allowlist for this Grok bridge.", ref(m)); err != nil {
			log.Printf("error: reply allowlist deny: %v", err)
		}
		return
	}

	parsed := ParseMessage(m.Content, s.State.User.ID)
	log.Printf("parse: kind=%s prompt=%q", kindName(parsed.Kind), truncate(parsed.Prompt, 300))

	switch parsed.Kind {
	case KindEmpty, KindHelp:
		if _, err := s.ChannelMessageSendReply(m.ChannelID, HelpText(), ref(m)); err != nil {
			log.Printf("error: reply help: %v", err)
		}
	case KindProjects:
		parentID := parentChannelID(s, m.ChannelID)
		msg := b.channelProjectHelp(parentID)
		if _, err := s.ChannelMessageSendReply(m.ChannelID, msg, ref(m)); err != nil {
			log.Printf("error: reply projects: %v", err)
		}
	case KindReset:
		if !isThread(s, m.ChannelID) {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /reset` inside a Grok thread.", ref(m)); err != nil {
				log.Printf("error: reply reset-not-thread: %v", err)
			}
			return
		}
		if err := b.sessions.Delete(m.ChannelID); err != nil {
			log.Printf("error: session delete: %v", err)
		}
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Session cleared for this thread.", ref(m)); err != nil {
			log.Printf("error: reply reset: %v", err)
		}
	case KindStatus:
		if !isThread(s, m.ChannelID) {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /status` inside a Grok thread.", ref(m)); err != nil {
				log.Printf("error: reply status-not-thread: %v", err)
			}
			return
		}
		e, ok := b.sessions.Get(m.ChannelID)
		if !ok {
			if _, err := s.ChannelMessageSendReply(m.ChannelID, "No session for this thread yet.", ref(m)); err != nil {
				log.Printf("error: reply status-empty: %v", err)
			}
			return
		}
		state := "idle"
		if _, busy := b.busy.Load(m.ChannelID); busy {
			state = "running"
		}
		if _, err := s.ChannelMessageSendReply(m.ChannelID, strings.Join([]string{
			"**project:** " + e.Project,
			"**cwd:** `" + e.Cwd + "`",
			"**session:** `" + e.SessionID + "`",
			"**updated:** " + e.UpdatedAt,
			"**state:** " + state,
		}, "\n"), ref(m)); err != nil {
			log.Printf("error: reply status: %v", err)
		}
	case KindTask:
		log.Printf("task: starting async for msg=%s", m.ID)
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("error: panic in handleTask msg=%s: %v", m.ID, r)
		}
	}()

	parentID := parentChannelID(s, m.ChannelID)
	log.Printf("task: msg=%s channel=%s parent=%s prompt=%q",
		m.ID, m.ChannelID, parentID, truncate(parsed.Prompt, 300))

	proj, err := b.resolveProject(parentID)
	if err != nil {
		log.Printf("error: resolve project parent=%s: %v", parentID, err)
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply resolve-project: %v", sendErr)
		}
		return
	}
	log.Printf("task: project=%s cwd=%s", proj.Name, proj.Cwd)

	title := threadNameFromPrompt(parsed.Prompt, m.Author.Username)
	needTitle := !isThread(s, m.ChannelID) || shouldRetitleThread(s, m.ChannelID)
	if needTitle && b.cfg.SummarizeTitleEnabled() {
		log.Printf("task: summarizing title via grok…")
		sumCtx, cancel := context.WithTimeout(context.Background(), time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond)
		if t, ok := grokrun.SummarizeTitle(sumCtx, b.cfg.GrokBin, b.cfg.Model, parsed.Prompt, proj.Cwd, time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond); ok {
			title = threadNameFromPrompt(t, m.Author.Username)
			log.Printf("task: grok title=%q", title)
		} else {
			log.Printf("task: summarize failed, using local title=%q", title)
		}
		cancel()
	}

	threadID, err := b.ensureThread(s, m, title)
	if err != nil {
		log.Printf("error: ensure thread: %v", err)
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, "Could not open thread: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply ensure-thread: %v", sendErr)
		}
		return
	}
	log.Printf("task: thread=%s title=%q", threadID, title)

	if _, loaded := b.busy.LoadOrStore(threadID, struct{}{}); loaded {
		log.Printf("task: busy thread=%s", threadID)
		if _, sendErr := s.ChannelMessageSend(threadID, "Already working in this thread — wait for the current run to finish."); sendErr != nil {
			log.Printf("error: reply busy: %v", sendErr)
		}
		return
	}
	defer b.busy.Delete(threadID)

	status, err := s.ChannelMessageSend(threadID, fmt.Sprintf("Working in **%s** (`%s`)…", proj.Name, proj.Cwd))
	if err != nil {
		log.Printf("error: status message thread=%s: %v", threadID, err)
		return
	}

	var sessionID string
	if e, ok := b.sessions.Get(threadID); ok {
		sessionID = e.SessionID
		log.Printf("task: resume session=%s", sessionID)
	}

	start := time.Now()
	log.Printf("task: running grok bin=%s yolo=%v maxTurns=%d timeout=%s",
		b.cfg.GrokBin, b.cfg.YoloEnabled(), b.cfg.MaxTurns, time.Duration(b.cfg.TimeoutMs)*time.Millisecond)

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

	log.Printf("task: grok done elapsed=%s code=%d session=%s textLen=%d stderrLen=%d text=%q",
		time.Since(start).Round(time.Millisecond),
		result.Code,
		result.SessionID,
		len(result.Text),
		len(result.Stderr),
		truncate(result.Text, 400),
	)
	if result.Stderr != "" {
		log.Printf("task: grok stderr=%q", truncate(result.Stderr, 2000))
	}
	if result.Code != 0 {
		log.Printf("error: grok exit code=%d", result.Code)
	}

	if result.SessionID != "" {
		if err := b.sessions.Set(threadID, sessionstore.Entry{
			SessionID: result.SessionID,
			Project:   proj.Name,
			Cwd:       proj.Cwd,
			LastUser:  m.Author.String(),
		}); err != nil {
			log.Printf("error: session save: %v", err)
		}
	}

	header := fmt.Sprintf("Done · **%s**", proj.Name)
	if result.Code != 0 {
		header = fmt.Sprintf("Finished with exit **%d** · **%s**", result.Code, proj.Name)
	}
	if _, err := s.ChannelMessageEdit(threadID, status.ID, header); err != nil {
		log.Printf("error: edit status: %v", err)
	}

	sendChunks(s, threadID, result.Text)

	if result.Stderr != "" && os.Getenv("GROK_DISCORD_DEBUG") != "" {
		errText := result.Stderr
		if len(errText) > 1500 {
			errText = errText[:1500]
		}
		sendChunks(s, threadID, "stderr:\n```\n"+errText+"\n```")
	}
	log.Printf("task: finished msg=%s thread=%s", m.ID, threadID)
}

// ensureThread creates or reuses a thread. name is the final Discord title
// (already summarized or locally trimmed).
func (b *Bot) ensureThread(s *discordgo.Session, m *discordgo.MessageCreate, name string) (string, error) {
	name = threadNameFromPrompt(name, m.Author.Username)

	if isThread(s, m.ChannelID) {
		// Keep follow-up replies in the same thread; only rename if still generic.
		if shouldRetitleThread(s, m.ChannelID) {
			if _, err := s.ChannelEdit(m.ChannelID, &discordgo.ChannelEdit{Name: name}); err != nil {
				log.Printf("warn: rename thread %s: %v", m.ChannelID, err)
			} else {
				log.Printf("task: renamed thread %s → %q", m.ChannelID, name)
			}
		}
		return m.ChannelID, nil
	}

	th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		return "", fmt.Errorf("MessageThreadStartComplex: %w", err)
	}
	log.Printf("task: created thread %s name=%q", th.ID, name)
	return th.ID, nil
}

// threadNameFromPrompt builds a Discord thread title (max 100 chars).
func threadNameFromPrompt(prompt, username string) string {
	summary := strings.Join(strings.Fields(prompt), " ")
	summary = strings.TrimSpace(summary)
	// Drop common leading noise.
	for _, p := range []string{"please ", "can you ", "could you ", "hey ", "hi "} {
		if len(summary) > len(p) && strings.EqualFold(summary[:len(p)], p) {
			summary = strings.TrimSpace(summary[len(p):])
		}
	}
	if summary == "" {
		summary = "task from " + username
	}

	const max = 100
	if len(summary) <= max {
		return summary
	}
	// Prefer cut on a word boundary.
	cut := strings.LastIndex(summary[:max-1], " ")
	if cut < max/3 {
		cut = max - 1
	}
	return strings.TrimSpace(summary[:cut]) + "…"
}

func shouldRetitleThread(s *discordgo.Session, channelID string) bool {
	ch, err := s.State.Channel(channelID)
	if err != nil {
		ch, err = s.Channel(channelID)
		if err != nil {
			return false
		}
	}
	name := strings.ToLower(strings.TrimSpace(ch.Name))
	// Retitle only placeholder / username-style titles from older runs.
	return name == "" ||
		strings.HasPrefix(name, "grok:") ||
		strings.HasPrefix(name, "task from ")
}

func kindName(k Kind) string {
	switch k {
	case KindEmpty:
		return "empty"
	case KindHelp:
		return "help"
	case KindProjects:
		return "projects"
	case KindReset:
		return "reset"
	case KindStatus:
		return "status"
	case KindTask:
		return "task"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
	log.Printf("reply: channel=%s parts=%d totalLen=%d", channelID, len(parts), len(text))
	for i, p := range parts {
		content := p
		if len(parts) > 1 {
			content = fmt.Sprintf("(%d/%d)\n%s", i+1, len(parts), p)
		}
		if _, err := s.ChannelMessageSend(channelID, content); err != nil {
			log.Printf("error: send chunk %d/%d channel=%s: %v", i+1, len(parts), channelID, err)
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


