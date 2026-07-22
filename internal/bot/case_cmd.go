package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// handleCase: @Grok /case [severity] <title>  or  /case ref:ID <title>
func (b *Bot) handleCase(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	parentID := parentChannelID(s, m.ChannelID)
	proj, err := b.resolveProject(parentID)
	if err != nil {
		replyText(s, m, err.Error())
		return
	}
	// Capability: Investigate or FileEscalation
	roleIDs := memberRoles(m)
	if b.cfg != nil {
		caps := b.cfg.ResolveCapabilities(proj.Name, m.Author.ID, roleIDs)
		if !caps.Investigate && !caps.FileEscalation && !caps.StartSessions {
			replyText(s, m, "You're not allowed to open cases on this project.")
			return
		}
	}

	severity, ref, title := parseCaseArgs(parsed.Prompt)
	if title == "" {
		replyText(s, m, "Usage: `@Grok /case [low|medium|high|critical] [ref:ID] <customer-facing title>`")
		return
	}

	threadID := m.ChannelID
	// Prefer working inside an existing Grok thread; otherwise ensureThread later on first investigate.
	if !isThread(s, m.ChannelID) {
		// Create a case thread from the parent message so we get a durable unit id.
		th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
			Name:                clampThreadTitle("Case · " + title),
			AutoArchiveDuration: 1440,
		})
		if err != nil {
			replyText(s, m, "Could not open case thread: "+err.Error())
			return
		}
		threadID = th.ID
	}

	actor := ActorFromUser(m.Author)
	if err := b.ensureCaseShell(threadID, proj.Name, actor, severity, ref, title, "discord"); err != nil {
		replyText(s, m, "Could not open case: "+err.Error())
		return
	}
	b.bindThreadOwnerActor(threadID, proj.Name, actor)

	body := formatCaseCard(severity, title, ref, sessionstore.PhaseIntake, "")
	msg, err := s.ChannelMessageSend(threadID, sanitizeDiscordContent(body))
	if err != nil {
		log.Printf("error: case card: %v", err)
	} else {
		_, _, _ = b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
			e.CaseMsgID = msg.ID
		})
	}
	if threadID != m.ChannelID {
		replyText(s, m, fmt.Sprintf("Opened case in thread (phase **intake**). Use `@Grok /investigate …` there."))
	} else {
		replyText(s, m, "Case set to **intake**. Next: `@Grok /investigate <notes>` or freeform (promotes to investigate).")
	}
}

func (b *Bot) ensureCaseShell(threadID, project string, actor Actor, severity, ref, title, source string) error {
	if b.sessions == nil {
		return fmt.Errorf("no session store")
	}
	e, ok := b.sessions.Get(threadID)
	if !ok {
		e = sessionstore.Entry{
			Project: project,
			Origin:  source,
		}
	}
	e.Mode = ModeCase
	e.Phase = sessionstore.PhaseIntake
	e.Severity = severity
	e.CustomerTitle = title
	e.CustomerRef = ref
	e.ReporterID = actor.ID
	e.ReporterName = actor.String()
	e.IntakeSource = source
	if e.Goal == "" {
		e.Goal = title
	}
	if e.Label == "" {
		e.Label = sessionstore.LabelOpen
	}
	if err := sessionstore.ClampCaseFields(&e); err != nil {
		return err
	}
	return b.sessions.Set(threadID, e)
}

func (b *Bot) handleEscalate(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /escalate` inside a case thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok || !e.IsCase() {
		replyText(s, m, "This thread is not a case. Open with `@Grok /case …` first.")
		return
	}
	if e.IsCaseClosed() {
		replyText(s, m, "Case is closed. `@Grok /reopen` is not implemented — open a new case or ask eng to `/label`.")
		return
	}
	roleIDs := memberRoles(m)
	if b.cfg != nil {
		caps := b.cfg.ResolveCapabilities(e.Project, m.Author.ID, roleIDs)
		if !caps.FileEscalation && !caps.GithubWrites && !caps.StartSessions {
			replyText(s, m, "You're not allowed to escalate cases (need fileEscalation or builder caps).")
			return
		}
	}
	note := strings.TrimSpace(parsed.Prompt)
	// strip command prefix if present
	note = stripCmdPrefix(note, "/escalate", "escalate")
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase // never clear
		ent.Phase = sessionstore.PhaseFixing
		ent.EscalatedAt = now
		if m.Author != nil {
			ent.EscalatedBy = m.Author.ID
		}
		if note != "" {
			if ent.Dossier == nil {
				ent.Dossier = &sessionstore.Dossier{}
			}
			ent.Dossier.NextActions = append(ent.Dossier.NextActions, "Escalate note: "+note)
		}
		if ent.Label == sessionstore.LabelBlocked || ent.Label == sessionstore.LabelOpen {
			ent.Label = sessionstore.LabelInProgress
		}
		_ = sessionstore.ClampCaseFields(ent)
	})
	if err != nil {
		replyText(s, m, "Escalate failed: "+err.Error())
		return
	}
	replyText(s, m, "Escalated → phase **fixing** (Mode stays **case**). Eng: freeform or `@Grok /start fix …` to implement. Escalation package will prefix the next ship run.")
}

func (b *Bot) handleCloseCase(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /close` inside a case thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok || !e.IsCase() {
		replyText(s, m, "This thread is not a case.")
		return
	}
	if !b.canControlThread(s, m, e) && !b.isModerator(s, m) {
		// Investigators who own the case can close
		if e.OwnerID != "" && m.Author != nil && e.OwnerID != m.Author.ID {
			replyText(s, m, "Only the case owner, co-owner, or a mod can close.")
			return
		}
	}
	res, note := parseCloseArgs(parsed.Prompt)
	if res == "" {
		res = "answered"
	}
	label := sessionstore.LabelDone
	switch res {
	case "wontfix", "escalated_external":
		label = sessionstore.LabelAbandoned
	case "fixed", "answered", "duplicate":
		label = sessionstore.LabelDone
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = sessionstore.PhaseClosed
		ent.Resolution = res
		ent.ResolutionNote = note
		ent.ResolvedAt = now
		if m.Author != nil {
			ent.ResolvedBy = m.Author.ID
		}
		ent.Label = label
		// K18: do NOT set LabelManual — closed phase freezes auto-label in sessionstore.
		_ = sessionstore.ClampCaseFields(ent)
	})
	if err != nil {
		replyText(s, m, "Close failed: "+err.Error())
		return
	}
	replyText(s, m, fmt.Sprintf("Case **closed** · resolution `%s` · label `%s`. PR auto-label will not reopen this case.", res, label))
}

func (b *Bot) handleCustomerUpdate(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /customer-update` inside a case thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok || !e.IsCase() {
		replyText(s, m, "This thread is not a case.")
		return
	}
	raw := stripCmdPrefix(parsed.Prompt, "/customer-update", "customer-update", "/update", "update")
	if raw == "" {
		// Show current
		cur := e.CustomerUpdate
		if cur == "" {
			replyText(s, m, "No customer update yet. Usage: `@Grok /customer-update <text>`")
			return
		}
		replyText(s, m, "**Customer update:**\n"+cur)
		return
	}
	clean, hits := SanitizeCustomerUpdate(raw)
	if clean == "" {
		replyText(s, m, "Customer update empty after sanitizer.")
		return
	}
	_, _, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		ent.CustomerUpdate = clean
		_ = sessionstore.ClampCaseFields(ent)
	})
	if err != nil {
		replyText(s, m, "Save failed: "+err.Error())
		return
	}
	msg := "**Customer update** (sanitized"
	if len(hits) > 0 {
		msg += "; redacted: " + strings.Join(hits, ", ")
	}
	msg += "):\n" + clean
	replyText(s, m, msg)
}

func (b *Bot) handleAnswer(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /answer` inside a case thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok || !e.IsCase() {
		replyText(s, m, "This thread is not a case.")
		return
	}
	note := stripCmdPrefix(parsed.Prompt, "/answer", "answer")
	_, _, err := b.sessions.Patch(m.ChannelID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = sessionstore.PhaseAnswered
		ent.Label = sessionstore.LabelBlocked // waiting on customer / knowledge close pending
		if note != "" {
			clean, _ := SanitizeCustomerUpdate(note)
			if clean != "" {
				ent.CustomerUpdate = clean
			}
		}
		_ = sessionstore.ClampCaseFields(ent)
	})
	if err != nil {
		replyText(s, m, "Answer failed: "+err.Error())
		return
	}
	replyText(s, m, "Phase → **answered** (label blocked). Set customer text with `/customer-update` then `/close answered` when done.")
}

func parseCaseArgs(prompt string) (severity, ref, title string) {
	text := stripCmdPrefix(prompt, "/case", "case")
	fields := strings.Fields(text)
	severity = "medium"
	var rest []string
	for _, f := range fields {
		fl := strings.ToLower(f)
		switch fl {
		case "low", "medium", "high", "critical", "sev1", "sev2", "sev3", "sev4":
			severity = normalizeSeverity(fl)
			continue
		}
		if strings.HasPrefix(fl, "ref:") {
			ref = strings.TrimSpace(f[4:])
			continue
		}
		rest = append(rest, f)
	}
	title = strings.TrimSpace(strings.Join(rest, " "))
	return severity, ref, title
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(s) {
	case "sev1", "critical":
		return "critical"
	case "sev2", "high":
		return "high"
	case "sev4", "low":
		return "low"
	default:
		return "medium"
	}
}

func parseCloseArgs(prompt string) (resolution, note string) {
	text := stripCmdPrefix(prompt, "/close", "close")
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "answered", ""
	}
	res := strings.ToLower(fields[0])
	switch res {
	case "answered", "fixed", "duplicate", "wontfix", "escalated_external":
		note = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		return res, note
	default:
		return "answered", text
	}
}

func stripCmdPrefix(text string, prefixes ...string) string {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)
	for _, p := range prefixes {
		pl := strings.ToLower(p)
		if lower == pl {
			return ""
		}
		if strings.HasPrefix(lower, pl+" ") {
			return strings.TrimSpace(t[len(p):])
		}
	}
	return t
}

func formatCaseCard(severity, title, ref, phase, extra string) string {
	var b strings.Builder
	b.WriteString("**Case** · phase `")
	b.WriteString(phase)
	b.WriteString("` · severity `")
	b.WriteString(severity)
	b.WriteString("`\n")
	b.WriteString("**Title:** ")
	b.WriteString(title)
	b.WriteString("\n")
	if ref != "" {
		b.WriteString("**Ref:** ")
		b.WriteString(ref)
		b.WriteString("\n")
	}
	b.WriteString("_Mode=case · investigate does not open PRs or direct-ship_\n")
	if extra != "" {
		b.WriteString(extra)
	}
	return b.String()
}

func clampThreadTitle(s string) string {
	r := []rune(s)
	if len(r) > 90 {
		return string(r[:87]) + "…"
	}
	return s
}

func memberRoles(m *discordgo.MessageCreate) []string {
	if m != nil && m.Member != nil {
		return m.Member.Roles
	}
	return nil
}

func replyText(s *discordgo.Session, m *discordgo.MessageCreate, text string) {
	if _, err := s.ChannelMessageSendReply(m.ChannelID, sanitizeDiscordContent(text), ref(m)); err != nil {
		log.Printf("error: reply: %v", err)
	}
}

// promoteCasePhaseBeforeRun updates case phase before investigate freeform (K19 order).
func (b *Bot) promoteCasePhaseBeforeRun(threadID string, toPhase string) {
	if b.sessions == nil {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || !e.IsCase() {
		return
	}
	if e.IsCaseClosed() {
		return
	}
	_, _, _ = b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = toPhase
		if toPhase == sessionstore.PhaseInvestigate && (ent.Label == "" || ent.Label == sessionstore.LabelOpen) {
			ent.Label = sessionstore.LabelInProgress
		}
		if toPhase == sessionstore.PhaseInvestigate && ent.Label == sessionstore.LabelBlocked {
			ent.Label = sessionstore.LabelInProgress // re-open from answered
		}
	})
}
