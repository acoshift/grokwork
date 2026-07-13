package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/grokrun"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

const (
	maxMsg          = 1900
	progressInterval = 15 * time.Second
)

// runJob is an in-flight Grok run for one Discord thread.
type runJob struct {
	cancel  context.CancelFunc
	start   time.Time
	project string
}

type Bot struct {
	cfg      *config.Config
	sessions *sessionstore.Store
	busy     sync.Map // threadID → *runJob
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
	// Bare @mention with files only still counts as a task.
	if parsed.Kind == KindEmpty && len(m.Attachments) > 0 {
		parsed = Parsed{Kind: KindTask, Prompt: "Please review the attached files."}
	}
	log.Printf("parse: kind=%s prompt=%q attachments=%d",
		kindName(parsed.Kind), truncate(parsed.Prompt, 300), len(m.Attachments))

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
		if job, busy := b.getJob(m.ChannelID); busy {
			state = "running · " + formatElapsed(time.Since(job.start))
		}
		if _, err := s.ChannelMessageSendReply(m.ChannelID, strings.Join([]string{
			"**project:** " + e.Project,
			"**session:** `" + e.SessionID + "`",
			"**updated:** " + e.UpdatedAt,
			"**state:** " + state,
		}, "\n"), ref(m)); err != nil {
			log.Printf("error: reply status: %v", err)
		}
	case KindCancel:
		b.handleCancel(s, m)
	case KindTask:
		log.Printf("task: starting async for msg=%s", m.ID)
		go b.handleTask(s, m, parsed)
	}
}

func (b *Bot) getJob(threadID string) (*runJob, bool) {
	v, ok := b.busy.Load(threadID)
	if !ok {
		return nil, false
	}
	job, ok := v.(*runJob)
	return job, ok
}

func (b *Bot) handleCancel(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /cancel` inside a Grok thread that is running.", ref(m)); err != nil {
			log.Printf("error: reply cancel-not-thread: %v", err)
		}
		return
	}
	job, ok := b.getJob(m.ChannelID)
	if !ok {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "No run in progress for this thread.", ref(m)); err != nil {
			log.Printf("error: reply cancel-idle: %v", err)
		}
		return
	}
	log.Printf("cancel: thread=%s project=%s elapsed=%s user=%s",
		m.ChannelID, job.project, formatElapsed(time.Since(job.start)), m.Author.String())
	job.cancel()
	if _, err := s.ChannelMessageSendReply(m.ChannelID, "Cancelling current run…", ref(m)); err != nil {
		log.Printf("error: reply cancel: %v", err)
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
	return fmt.Sprintf("This channel → **%s**", proj.Name)
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

	titlePrompt := parsed.Prompt
	if titlePrompt == "" && len(m.Attachments) > 0 {
		titlePrompt = "attachments: " + m.Attachments[0].Filename
	}
	title := threadNameFromPrompt(titlePrompt, m.Author.Username)
	needTitle := !isThread(s, m.ChannelID) || shouldRetitleThread(s, m.ChannelID)
	if needTitle && b.cfg.SummarizeTitleEnabled() {
		log.Printf("task: summarizing title via grok…")
		sumCtx, cancel := context.WithTimeout(context.Background(), time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond)
		if t, ok := grokrun.SummarizeTitle(sumCtx, b.cfg.GrokBin, b.cfg.Model, titlePrompt, proj.Cwd, time.Duration(b.cfg.SummarizeTimeoutMs)*time.Millisecond); ok {
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

	ctx, cancel := context.WithCancel(context.Background())
	job := &runJob{cancel: cancel, start: time.Now(), project: proj.Name}
	if _, loaded := b.busy.LoadOrStore(threadID, job); loaded {
		cancel()
		log.Printf("task: busy thread=%s", threadID)
		if _, sendErr := s.ChannelMessageSend(threadID, "Already working in this thread — wait for the current run to finish, or `@Grok /cancel`."); sendErr != nil {
			log.Printf("error: reply busy: %v", sendErr)
		}
		return
	}
	defer func() {
		cancel()
		b.busy.Delete(threadID)
	}()

	status, err := s.ChannelMessageSend(threadID, workingStatus(proj.Name, 0))
	if err != nil {
		log.Printf("error: status message thread=%s: %v", threadID, err)
		return
	}

	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		b.progressLoop(s, threadID, status.ID, proj.Name, job.start, stopProgress)
	}()

	prompt := parsed.Prompt
	if len(m.Attachments) > 0 {
		attDir := filepath.Join(b.cfg.DataDir, "attachments", m.ID)
		defer func() {
			if rmErr := os.RemoveAll(attDir); rmErr != nil {
				log.Printf("warn: cleanup attachments %s: %v", attDir, rmErr)
			}
		}()
		log.Printf("task: downloading %d attachment(s) → %s", len(m.Attachments), attDir)
		files, dlErr := downloadAttachments(ctx, m.Attachments, attDir)
		if dlErr != nil {
			close(stopProgress)
			progressWG.Wait()
			log.Printf("error: attachments: %v", dlErr)
			msg := "Could not download attachments: " + dlErr.Error()
			if _, editErr := s.ChannelMessageEdit(threadID, status.ID, "Failed · attachments"); editErr != nil {
				log.Printf("error: edit status: %v", editErr)
			}
			sendChunks(s, threadID, msg)
			return
		}
		prompt = promptWithAttachments(prompt, files)
		log.Printf("task: saved %d attachment(s)", len(files))
	}

	var sessionID string
	if e, ok := b.sessions.Get(threadID); ok {
		sessionID = e.SessionID
		log.Printf("task: resume session=%s", sessionID)
	}

	log.Printf("task: running grok bin=%s yolo=%v maxTurns=%d timeout=%s",
		b.cfg.GrokBin, b.cfg.YoloEnabled(), b.cfg.MaxTurns, time.Duration(b.cfg.TimeoutMs)*time.Millisecond)

	result := grokrun.Run(ctx, grokrun.Options{
		GrokBin:   b.cfg.GrokBin,
		Prompt:    prompt,
		Cwd:       proj.Cwd,
		SessionID: sessionID,
		Yolo:      b.cfg.YoloEnabled(),
		Model:     b.cfg.Model,
		MaxTurns:  b.cfg.MaxTurns,
		Timeout:   time.Duration(b.cfg.TimeoutMs) * time.Millisecond,
		ExtraArgs: b.cfg.ExtraArgs,
	})

	close(stopProgress)
	progressWG.Wait()

	elapsed := time.Since(job.start)
	log.Printf("task: grok done elapsed=%s code=%d cancelled=%v session=%s textLen=%d stderrLen=%d text=%q",
		elapsed.Round(time.Millisecond),
		result.Code,
		result.Cancelled,
		result.SessionID,
		len(result.Text),
		len(result.Stderr),
		truncate(result.Text, 400),
	)
	if result.Stderr != "" {
		log.Printf("task: grok stderr=%q", truncate(result.Stderr, 2000))
	}
	if result.Code != 0 && !result.Cancelled {
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

	header := fmt.Sprintf("Done · **%s** · %s", proj.Name, formatElapsed(elapsed))
	switch {
	case result.Cancelled:
		header = fmt.Sprintf("Cancelled · **%s** · %s", proj.Name, formatElapsed(elapsed))
	case result.Code != 0:
		header = fmt.Sprintf("Finished with exit **%d** · **%s** · %s", result.Code, proj.Name, formatElapsed(elapsed))
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

// progressLoop edits the status message with elapsed time until stop is closed.
func (b *Bot) progressLoop(s *discordgo.Session, threadID, msgID, project string, start time.Time, stop <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			text := workingStatus(project, time.Since(start))
			if _, err := s.ChannelMessageEdit(threadID, msgID, text); err != nil {
				log.Printf("warn: progress edit thread=%s: %v", threadID, err)
			}
		}
	}
}

func workingStatus(project string, elapsed time.Duration) string {
	if elapsed < time.Second {
		return fmt.Sprintf("Working in **%s**… · `@Grok /cancel` to stop", project)
	}
	return fmt.Sprintf("Working in **%s**… · %s elapsed · `@Grok /cancel` to stop",
		project, formatElapsed(elapsed))
}

// formatElapsed renders a compact duration for Discord status lines.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
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
	case KindCancel:
		return "cancel"
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


