package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/sessionstore"
)

const maxBoardMsgRunes = 1800
const maxBoardRows = 40

// preserveLabelFields copies lifecycle label when session Set overwrites the entry.
func preserveLabelFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.Label == "" {
		next.Label = prev.Label
		next.LabelManual = prev.LabelManual
	}
}

func (b *Bot) handleLabel(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /label` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply label-not-thread: %v", err)
		}
		return
	}
	threadID := m.ChannelID
	arg := parseLabelArg(parsed.Prompt)

	e, ok := b.sessions.Get(threadID)
	if !ok {
		// Shell so label sticks before first task.
		parentID := parentChannelID(s, threadID)
		projName := ""
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
		e = sessionstore.Entry{Project: projName}
		if m.Author != nil {
			ensureSessionOwner(&e, m.Author.ID, m.Author.String())
		}
	}

	switch {
	case arg == "":
		msg := formatLabelStatus(e)
		if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
			log.Printf("error: reply label-status: %v", err)
		}
		return
	case arg == "auto":
		e.ClearLabelManual()
		if err := b.sessions.Set(threadID, e); err != nil {
			if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save label: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply label-save: %v", sendErr)
			}
			return
		}
		msg := fmt.Sprintf("Label auto-enabled → **%s**.", sessionstore.DisplayLabel(e.EffectiveLabel()))
		if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
			log.Printf("error: reply label-auto: %v", err)
		}
		b.maybeRefreshBriefLabel(s, threadID)
		return
	case arg == "help" || arg == "?":
		if _, err := s.ChannelMessageSendReply(threadID, labelHelpText(), ref(m)); err != nil {
			log.Printf("error: reply label-help: %v", err)
		}
		return
	}

	lab, okLab := sessionstore.ParseLabel(arg)
	if !okLab {
		if _, err := s.ChannelMessageSendReply(threadID,
			fmt.Sprintf("Unknown label `%s`. %s", arg, labelHelpText()), ref(m)); err != nil {
			log.Printf("error: reply label-unknown: %v", err)
		}
		return
	}
	if err := e.SetLabelManual(lab); err != nil {
		if _, sendErr := s.ChannelMessageSendReply(threadID, err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply label-set: %v", sendErr)
		}
		return
	}
	if m.Author != nil {
		ensureSessionOwner(&e, m.Author.ID, m.Author.String())
	}
	if e.Project == "" {
		parentID := parentChannelID(s, threadID)
		if p, err := b.resolveProject(parentID); err == nil {
			e.Project = p.Name
		}
	}
	if err := b.sessions.Set(threadID, e); err != nil {
		if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save label: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply label-save: %v", sendErr)
		}
		return
	}
	msg := fmt.Sprintf("Label set to **%s** (manual — auto paused until `@Grok /label auto`).", sessionstore.DisplayLabel(lab))
	if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
		log.Printf("error: reply label-ok: %v", err)
	}
	b.maybeRefreshBriefLabel(s, threadID)
}

func parseLabelArg(prompt string) string {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, prefix := range []string{"/label", "label"} {
		if lower == prefix {
			return ""
		}
		if strings.HasPrefix(lower, prefix+" ") {
			return strings.TrimSpace(text[len(prefix):])
		}
	}
	return strings.TrimSpace(text)
}

func formatLabelStatus(e sessionstore.Entry) string {
	lab := sessionstore.DisplayLabel(e.EffectiveLabel())
	mode := "auto"
	if e.LabelManual {
		mode = "manual"
	}
	return fmt.Sprintf("**label:** %s (%s)\n%s", lab, mode, labelHelpText())
}

func labelHelpText() string {
	return "Set: `@Grok /label <open|in_progress|blocked|needs_review|done|abandoned>` · `@Grok /label auto` · aliases: wip, ready, review, close"
}

func (b *Bot) maybeRefreshBriefLabel(s *discordgo.Session, threadID string) {
	if b == nil || s == nil || threadID == "" {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || e.BriefMsgID == "" {
		return
	}
	if _, err := b.refreshBriefCard(s, threadID, ""); err != nil {
		log.Printf("label: brief refresh thread=%s: %v", threadID, err)
	}
}

// applyAutoLabelOnRunStart promotes open → in_progress when a task starts.
func (b *Bot) applyAutoLabelOnRunStart(threadID, project string, m *discordgo.MessageCreate) {
	if b == nil || b.sessions == nil || threadID == "" {
		return
	}
	if _, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if ent.Project == "" {
			ent.Project = project
		}
		if m != nil && m.Author != nil {
			ensureSessionOwner(ent, m.Author.ID, m.Author.String())
		}
		ent.ApplyAutoLabelOnRunStart()
	}); err != nil {
		log.Printf("label: run-start thread=%s: %v", threadID, err)
	} else if !ok {
		e := sessionstore.Entry{Project: project, Label: sessionstore.LabelInProgress}
		if m != nil && m.Author != nil {
			ensureSessionOwner(&e, m.Author.ID, m.Author.String())
		}
		if err := b.sessions.Set(threadID, e); err != nil {
			log.Printf("label: create on run-start thread=%s: %v", threadID, err)
		}
	}
}

func (b *Bot) handleBoard(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	projectFilter, labelFilter, includeTerminal, errMsg := parseBoardArgs(parsed.Prompt, b.knownProjects())
	if errMsg != "" {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, errMsg, ref(m)); err != nil {
			log.Printf("error: reply board-usage: %v", err)
		}
		return
	}

	rows := b.collectBoardRows(projectFilter, labelFilter, includeTerminal)
	body := formatBoardCard(rows, projectFilter, labelFilter, includeTerminal)
	if _, err := s.ChannelMessageSendReply(m.ChannelID, body, ref(m)); err != nil {
		log.Printf("error: reply board: %v", err)
	}
}

func (b *Bot) knownProjects() map[string]struct{} {
	out := map[string]struct{}{}
	if b == nil || b.cfg == nil {
		return out
	}
	for name := range b.cfg.Projects {
		out[strings.ToLower(name)] = struct{}{}
	}
	return out
}

// parseBoardArgs: /board [project] [label|all]
// project must match a configured project name; label uses ParseLabel (or "all").
func parseBoardArgs(prompt string, projects map[string]struct{}) (project, label string, includeTerminal bool, errMsg string) {
	text := strings.TrimSpace(prompt)
	lower := strings.ToLower(text)
	for _, prefix := range []string{"/board", "board"} {
		if lower == prefix {
			return "", "", false, ""
		}
		if strings.HasPrefix(lower, prefix+" ") {
			text = strings.TrimSpace(text[len(prefix):])
			lower = strings.ToLower(text)
			break
		}
	}
	if text == "" {
		return "", "", false, ""
	}
	fields := strings.Fields(lower)
	for _, f := range fields {
		if f == "all" {
			includeTerminal = true
			continue
		}
		if lab, ok := sessionstore.ParseLabel(f); ok {
			label = lab
			if sessionstore.IsTerminalLabel(lab) {
				includeTerminal = true
			}
			continue
		}
		if _, ok := projects[f]; ok {
			// Preserve canonical project casing from config via caller; store filter lower.
			project = f
			continue
		}
		return "", "", false, fmt.Sprintf(
			"Unknown board filter `%s`. Use `@Grok /board [project] [label|all]`.\nLabels: open, in_progress, blocked, needs_review, done, abandoned.",
			f,
		)
	}
	return project, label, includeTerminal, ""
}

type boardRow struct {
	ThreadID string
	Project  string
	Label    string
	Goal     string
	OwnerID  string
	Running  bool
	Queue    int
}

func (b *Bot) collectBoardRows(projectFilter, labelFilter string, includeTerminal bool) []boardRow {
	if b == nil || b.sessions == nil {
		return nil
	}
	list := b.sessions.List()
	out := make([]boardRow, 0, len(list))
	for _, listed := range list {
		e := listed.Entry
		lab := e.EffectiveLabel()
		if labelFilter != "" && lab != labelFilter {
			continue
		}
		if !includeTerminal && sessionstore.IsTerminalLabel(lab) {
			continue
		}
		if projectFilter != "" && !strings.EqualFold(e.Project, projectFilter) {
			continue
		}
		goal := strings.TrimSpace(e.Goal)
		if goal == "" {
			goal = b.lastPromptPreview(listed.ThreadID)
		}
		row := boardRow{
			ThreadID: listed.ThreadID,
			Project:  e.Project,
			Label:    lab,
			Goal:     goal,
			OwnerID:  e.OwnerID,
			Queue:    b.queueLen(listed.ThreadID),
		}
		if _, busy := b.getJob(listed.ThreadID); busy {
			row.Running = true
		}
		out = append(out, row)
		if len(out) >= maxBoardRows {
			break
		}
	}
	return out
}

func formatBoardCard(rows []boardRow, projectFilter, labelFilter string, includeTerminal bool) string {
	var headParts []string
	headParts = append(headParts, "**Board**")
	if projectFilter != "" {
		headParts = append(headParts, "**"+projectFilter+"**")
	} else {
		headParts = append(headParts, "all projects")
	}
	if labelFilter != "" {
		headParts = append(headParts, sessionstore.DisplayLabel(labelFilter))
	} else if includeTerminal {
		headParts = append(headParts, "including done/abandoned")
	} else {
		headParts = append(headParts, "active")
	}

	if len(rows) == 0 {
		return strings.Join(headParts, " · ") + "\n_(no matching threads)_"
	}

	// Group by canonical label order.
	byLabel := map[string][]boardRow{}
	for _, r := range rows {
		byLabel[r.Label] = append(byLabel[r.Label], r)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("%s · %d thread%s", strings.Join(headParts, " · "), len(rows), plural(len(rows))))

	for _, lab := range sessionstore.CanonicalLabels {
		group := byLabel[lab]
		if len(group) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("**%s** (%d)", sessionstore.DisplayLabel(lab), len(group)))
		for _, r := range group {
			lines = append(lines, formatBoardLine(r))
		}
	}

	body := strings.Join(lines, "\n")
	return truncateRunes(body, maxBoardMsgRunes)
}

func formatBoardLine(r boardRow) string {
	proj := r.Project
	if proj == "" {
		proj = "?"
	}
	goal := strings.TrimSpace(r.Goal)
	if goal == "" {
		goal = "(no goal)"
	} else {
		goal = truncateRunes(goal, 80)
	}
	var bits []string
	bits = append(bits, fmt.Sprintf("<#%s>", r.ThreadID))
	bits = append(bits, "**"+proj+"**")
	bits = append(bits, goal)
	if r.OwnerID != "" {
		bits = append(bits, "<@"+r.OwnerID+">")
	}
	if r.Running {
		bits = append(bits, "_running_")
	} else if r.Queue > 0 {
		bits = append(bits, fmt.Sprintf("queue %d", r.Queue))
	}
	return "• " + strings.Join(bits, " · ")
}
