package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// caseAuditDetail stamps the case's own identifiers onto an audit detail map.
//
// CaseKey is the quotable id an operator will search by, and phase says what the
// case was when the command arrived — which is what makes a denial legible ("they
// tried to close an already-closed case"). CustomerTitle and CustomerUpdate are
// never included: they are the customer's words.
func caseAuditDetail(e sessionstore.Entry, extra map[string]any) map[string]any {
	d := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		d[k] = v
	}
	d["caseKey"] = e.CaseKey
	d["phase"] = e.CasePhase()
	return d
}

// handleCase: @Grok /case [severity] <title>  or  /case ref:ID <title>
func (b *Bot) handleCase(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	parentID := parentChannelID(s, m.ChannelID)
	proj, err := b.resolveProject(parentID)
	if err != nil {
		replyText(s, m, err.Error())
		return
	}
	// Capability: Investigate or FileEscalation
	if b.cfg != nil {
		caps := b.cfg.ResolveCapabilities(proj.Name, m.Author.ID)
		if !caps.Investigate && !caps.FileEscalation && !caps.StartSessions {
			b.auditCmdMsg(audit.ActionSessionStart, m, proj.Name, errAuditDeniedCapability,
				map[string]any{"origin": "discord-case"})
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

	// Refuse clobber: closed cases and non-case eng threads.
	if e, ok := b.sessions.Get(threadID); ok {
		if e.IsCaseClosed() {
			replyText(s, m, "This case is **closed**. Use `@Grok /reopen` (or `/reopen fixing`) to resume, or open a new thread with `@Grok /case …`.")
			return
		}
		if e.Mode != "" && !e.IsCase() {
			replyText(s, m, "This thread is already a **"+e.Mode+"** session, not a case. Start `/case` in a new message outside this thread.")
			return
		}
	}
	actor := ActorFromUser(m.Author)
	// Detail carries the case's own ids and severity, never the customer-facing
	// title — that is the one field of an intake guaranteed to be their words.
	caseDetail := map[string]any{"origin": "discord-case", "severity": severity, "ref": ref}
	if err := b.ensureCaseShell(threadID, proj.Name, actor, severity, ref, title, "discord"); err != nil {
		b.auditCmd(audit.ActionSessionStart, actor, threadID, proj.Name, err, caseDetail)
		replyText(s, m, "Could not open case: "+err.Error())
		return
	}
	b.bindThreadOwnerActor(threadID, proj.Name, actor)
	if e, ok := b.sessions.Get(threadID); ok {
		caseDetail["caseKey"] = e.CaseKey
	}
	b.auditCmd(audit.ActionSessionStart, actor, threadID, proj.Name, nil, caseDetail)

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

// caseKeyPrefix resolves the prefix new keys for a project take: the project's
// configured override when it has one, otherwise its name. Normalising the
// result into a legal prefix is sessionstore's job.
func (b *Bot) caseKeyPrefix(project string) string {
	if b.cfg != nil {
		if custom := b.cfg.ProjectCaseKey(project); custom != "" {
			return custom
		}
	}
	return project
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
	// Minted once, here, because this is the one funnel both intake surfaces
	// pass through. Never re-minted: a second /case on the same thread (a
	// re-file with a corrected title) must not renumber a case someone has
	// already quoted. A failure to allocate is not fatal — a case without a
	// key is still a case, and the alternative is refusing to record an
	// incident because a counter file could not be written.
	if e.CaseKey == "" {
		prefix := b.caseKeyPrefix(project)
		if key, err := b.sessions.AllocateCaseKey(prefix); err != nil {
			log.Printf("error: allocate case key for %s: %v", project, err)
		} else {
			e.CaseKey = key
		}
	}
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
		replyText(s, m, "Case is closed. Use `@Grok /reopen` first.")
		return
	}
	// Builder-class means the escalator is engineering and takes the case; anyone
	// else is handing it to engineering, which leaves it unassigned.
	var caps config.Capabilities
	if b.cfg != nil {
		caps = b.cfg.ResolveCapabilities(e.Project, m.Author.ID)
		if !canEscalateCase(caps) {
			b.auditCmdMsg(audit.ActionCaseEscalate, m, e.Project, errAuditDeniedCapability, caseAuditDetail(e, nil))
			replyText(s, m, "You're not allowed to escalate cases (need fileEscalation or builder caps).")
			return
		}
	}
	note := strings.TrimSpace(parsed.Prompt)
	// strip command prefix if present
	note = stripCmdPrefix(note, "/escalate", "escalate")
	out, err := b.EscalateCase(EscalateCaseOpts{
		ThreadID:      m.ChannelID,
		Actor:         ActorFromUser(m.Author),
		Note:          note,
		TakeOwnership: caps.CanShip(),
	})
	// The note is the escalator's free text and stays out of the log; who now holds
	// the case is the part an operator needs.
	b.auditCmdMsg(audit.ActionCaseEscalate, m, e.Project, err, caseAuditDetail(e, map[string]any{
		"assigned":   out.Assigned,
		"released":   out.Released,
		"engineerId": out.EngineerID,
	}))
	if err != nil {
		replyText(s, m, "Escalate failed: "+err.Error())
		return
	}
	// Phrased from the outcome, not from caps — see EscalateOutcome.
	reply := "Escalated → phase **fixing** (Mode stays **case**)."
	switch {
	case out.Assigned:
		reply += " Assigned to you."
	case out.EngineerID != "":
		reply += " Still assigned to the same engineer."
	default:
		reply += "\nNo engineer assigned yet — it shows on the case board under **needs an engineer**."
	}
	reply += "\nEng: freeform or `@Grok /start fix …` to implement. Escalation package will prefix the next ship run."
	replyText(s, m, reply)
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
	if !b.canControlThread(m, e) {
		// Investigators who own the case can close
		if e.OwnerID != "" && m.Author != nil && e.OwnerID != m.Author.ID {
			b.auditCmdMsg(audit.ActionCaseClose, m, e.Project, errAuditDeniedControl, caseAuditDetail(e, nil))
			replyText(s, m, "Only the case owner, co-owner, or a project admin can close.")
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
	// resolution/label are our own enums; the close note is the operator's prose
	// about a customer and is not logged.
	b.auditCmdMsg(audit.ActionCaseClose, m, e.Project, err, caseAuditDetail(e, map[string]any{
		"resolution": res,
		"label":      label,
	}))
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
	if e.IsCaseClosed() {
		replyText(s, m, "This case is **closed**. Customer text is frozen.")
		return
	}
	if b.cfg != nil && m.Author != nil {
		caps := b.cfg.ResolveCapabilities(e.Project, m.Author.ID)
		if !caps.DraftCustomerReply && !canEscalateCase(caps) {
			b.auditCmdMsg(audit.ActionCaseCustomerUpdate, m, e.Project, errAuditDeniedCapability, caseAuditDetail(e, nil))
			replyText(s, m, "You're not allowed to draft customer updates (need draftCustomerReply).")
			return
		}
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
	// The customer-facing text itself is the one thing this command is about and
	// the one thing that must not be copied here: it is written *to* a customer.
	// What the sanitizer had to redact is worth knowing, so the count travels.
	b.auditCmdMsg(audit.ActionCaseCustomerUpdate, m, e.Project, err, caseAuditDetail(e, map[string]any{
		"redactions": len(hits),
	}))
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

func (b *Bot) handleReopenCase(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		replyText(s, m, "Use `@Grok /reopen` inside a case thread.")
		return
	}
	e, ok := b.sessions.Get(m.ChannelID)
	if !ok || !e.IsCase() {
		replyText(s, m, "This thread is not a case.")
		return
	}
	if !e.IsCaseClosed() {
		replyText(s, m, "This case is already open (phase **"+e.CasePhase()+"**).")
		return
	}
	allowed := b.canControlThread(m, e)
	if !allowed && b.cfg != nil && m.Author != nil {
		caps := b.cfg.ResolveCapabilities(e.Project, m.Author.ID)
		allowed = CanReopenCaseCaps(caps)
	}
	if !allowed {
		b.auditCmdMsg(audit.ActionCaseReopen, m, e.Project, errAuditDeniedCapability, caseAuditDetail(e, nil))
		replyText(s, m, "You're not allowed to reopen cases (need investigate / fileEscalation / startSessions, or own the case).")
		return
	}
	phase := parseReopenPhase(parsed.Prompt)
	if phase == "" {
		replyText(s, m, "Usage: `@Grok /reopen [investigate|fixing]` (default investigate).")
		return
	}
	actorID := ""
	if m.Author != nil {
		actorID = m.Author.ID
	}
	err := b.ReopenCase(m.ChannelID, actorID, phase)
	// toPhase, not phase: caseAuditDetail owns "phase" and it means the phase the
	// case was in when the command arrived (closed, here).
	b.auditCmdMsg(audit.ActionCaseReopen, m, e.Project, err, caseAuditDetail(e, map[string]any{"toPhase": phase}))
	if err != nil {
		if err == ErrCaseBadPhase {
			replyText(s, m, "Usage: `@Grok /reopen [investigate|fixing]` (default investigate).")
			return
		}
		replyText(s, m, "Reopen failed: "+err.Error())
		return
	}
	replyText(s, m, fmt.Sprintf("Case **reopened** · phase **%s** (Mode stays **case**; dossier preserved). Freeform and case actions work again.", phase))
}

func parseReopenPhase(prompt string) string {
	text := strings.ToLower(strings.TrimSpace(stripCmdPrefix(prompt, "/reopen", "reopen")))
	if text == "" {
		return sessionstore.PhaseInvestigate
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return sessionstore.PhaseInvestigate
	}
	switch fields[0] {
	case sessionstore.PhaseInvestigate, "inv":
		return sessionstore.PhaseInvestigate
	case sessionstore.PhaseFixing, "fix", "eng":
		return sessionstore.PhaseFixing
	default:
		return "" // invalid
	}
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
	if e.IsCaseClosed() {
		replyText(s, m, "This case is **closed**. Use `@Grok /reopen` first.")
		return
	}
	if b.cfg != nil && m.Author != nil {
		caps := b.cfg.ResolveCapabilities(e.Project, m.Author.ID)
		if !caps.DraftCustomerReply && !canEscalateCase(caps) {
			b.auditCmdMsg(audit.ActionCaseAnswer, m, e.Project, errAuditDeniedCapability, caseAuditDetail(e, nil))
			replyText(s, m, "You're not allowed to mark cases answered (need draftCustomerReply or escalate caps).")
			return
		}
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
	b.auditCmdMsg(audit.ActionCaseAnswer, m, e.Project, err, caseAuditDetail(e, nil))
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

func replyText(s *discordgo.Session, m *discordgo.MessageCreate, text string) {
	if _, err := discordReply(s, m.ChannelID, sanitizeDiscordContent(text), ref(m)); err != nil {
		log.Printf("error: reply: %v", err)
	}
}

// canEscalateCase matches /escalate and /start fix on Mode=case (K17).
// FileEscalation, GithubWrites, or StartSessions (builder-class) required.
func canEscalateCase(caps config.Capabilities) bool {
	return caps.FileEscalation || caps.GithubWrites || caps.StartSessions
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
