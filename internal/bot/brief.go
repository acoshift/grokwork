package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const (
	maxBriefMsgRunes     = 1800
	maxBriefGoalRunes    = 200
	maxBriefDoneTurns    = 4
	maxBriefFileLines    = 8
	maxBriefQuestions    = 3
	maxBriefPreviewRunes = 120
)

// BriefCardInput is everything needed to format the continuity / brief card.
type BriefCardInput struct {
	Project    string
	OwnerID    string
	OwnerName  string
	Goal       string
	Label      string // lifecycle: open / in progress / …
	LabelMode  string // auto | manual (empty = omit)
	Status     string // idle / running · …
	Turns      int
	Done       []string // recent completed turn previews
	Left       string
	Branch     string
	HeadShort  string
	IssueLines []string
	PRLines    []string
	Files      []string // name-status lines (status\tpath)
	Questions  []string
	Queue      int
}

// FormatBriefCard builds the Discord continuity card (no embeds).
func FormatBriefCard(in BriefCardInput) string {
	project := strings.TrimSpace(in.Project)
	if project == "" {
		project = "(unknown)"
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("**Brief** · **%s**", project))

	if in.OwnerID != "" {
		if in.OwnerName != "" {
			lines = append(lines, fmt.Sprintf("**owner:** %s (<@%s>)", in.OwnerName, in.OwnerID))
		} else {
			lines = append(lines, fmt.Sprintf("**owner:** <@%s>", in.OwnerID))
		}
	}

	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		goal = "(not set — first `@Grok` task or `/brief goal …`)"
	}
	lines = append(lines, "**goal:** "+goal)

	if lab := strings.TrimSpace(in.Label); lab != "" {
		if in.LabelMode == "manual" {
			lines = append(lines, "**label:** "+lab+" (manual)")
		} else {
			lines = append(lines, "**label:** "+lab)
		}
	}

	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "idle"
	}
	if in.Turns > 0 {
		lines = append(lines, fmt.Sprintf("**status:** %s · %d turn%s", status, in.Turns, plural(in.Turns)))
	} else {
		lines = append(lines, "**status:** "+status)
	}

	if len(in.Done) > 0 {
		lines = append(lines, "**done:**")
		for _, d := range in.Done {
			lines = append(lines, "• "+d)
		}
	} else {
		lines = append(lines, "**done:** (none yet)")
	}

	left := strings.TrimSpace(in.Left)
	if left == "" {
		left = "—"
	}
	lines = append(lines, "**left:** "+left)

	if in.Branch != "" || in.HeadShort != "" {
		b := in.Branch
		if b == "" {
			b = "(detached)"
		}
		if in.HeadShort != "" {
			lines = append(lines, fmt.Sprintf("**branch:** `%s` @ `%s`", b, in.HeadShort))
		} else {
			lines = append(lines, fmt.Sprintf("**branch:** `%s`", b))
		}
	}

	if len(in.IssueLines) > 0 {
		lines = append(lines, in.IssueLines...)
	}

	if len(in.PRLines) > 0 {
		lines = append(lines, in.PRLines...)
	} else {
		lines = append(lines, "**pr:** (none yet)")
	}

	if names := formatNameStatusLines(in.Files, maxBriefFileLines); names != "" {
		lines = append(lines, "**files:**")
		lines = append(lines, "```")
		lines = append(lines, names)
		lines = append(lines, "```")
	}

	if len(in.Questions) > 0 {
		lines = append(lines, "**questions:**")
		for _, q := range in.Questions {
			lines = append(lines, "• "+q)
		}
	}

	if in.Queue > 0 {
		lines = append(lines, fmt.Sprintf("**queue:** %d follow-up%s", in.Queue, plural(in.Queue)))
	}

	return truncateRunes(strings.Join(lines, "\n"), maxBriefMsgRunes)
}

func (b *Bot) handleBrief(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /brief` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply brief-not-thread: %v", err)
		}
		return
	}
	threadID := m.ChannelID

	// Optional: /brief goal <text> sets sticky goal then refreshes.
	if goal, ok := parseBriefGoalArg(parsed.Prompt); ok {
		if err := b.setThreadGoal(s, threadID, goal, m); err != nil {
			if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save goal: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply brief-goal: %v", sendErr)
			}
			return
		}
	}

	pinned, err := b.refreshBriefCard(s, threadID, "")
	if err != nil {
		log.Printf("brief: refresh thread=%s: %v", threadID, err)
		if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not update brief: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply brief-fail: %v", sendErr)
		}
		return
	}
	// Soft ack — the card may sit higher in the thread after pin/edit.
	ack := "Brief card updated."
	if pinned {
		ack = "Brief card updated and pinned."
	} else {
		// Pin needs Pin Messages (1<<51); Manage Messages alone is not enough.
		ack = "Brief card updated (not pinned — bot needs **Pin Messages**; re-authorize via the admin Config page install URL, or enable Pin Messages on the bot role)."
	}
	if _, err := s.ChannelMessageSendReply(threadID, ack, ref(m)); err != nil {
		log.Printf("error: reply brief-ok: %v", err)
	}
}

// parseBriefGoalArg extracts goal text from "/brief goal …" / "brief set goal …".
// Returns ok=false when the command is a plain refresh.
func parseBriefGoalArg(prompt string) (string, bool) {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	// Strip leading /brief or brief.
	for _, prefix := range []string{"/brief", "brief"} {
		if strings.HasPrefix(lower, prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			lower = strings.ToLower(text)
			break
		}
	}
	if text == "" {
		return "", false
	}
	// "goal …" or "set goal …"
	switch {
	case strings.HasPrefix(lower, "set goal "):
		return clampGoal(strings.TrimSpace(text[len("set goal "):])), true
	case strings.HasPrefix(lower, "goal "):
		return clampGoal(strings.TrimSpace(text[len("goal "):])), true
	case lower == "set goal" || lower == "goal":
		return "", true // explicit clear not supported; treat as empty no-op set
	default:
		// Unknown subcommand — treat whole remainder as free-form goal set? No: refresh only.
		return "", false
	}
}

func clampGoal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return truncateRunes(s, maxBriefGoalRunes)
}

func (b *Bot) setThreadGoal(s *discordgo.Session, threadID, goal string, m *discordgo.MessageCreate) error {
	if b == nil || b.sessions == nil || threadID == "" {
		return fmt.Errorf("no session store")
	}
	goal = clampGoal(goal)
	if goal == "" {
		return fmt.Errorf("goal text is empty — use `@Grok /brief goal <text>`")
	}
	if _, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Goal = goal
		if ent.Project == "" && s != nil {
			parentID := parentChannelID(s, threadID)
			if p, rErr := b.resolveProject(parentID); rErr == nil {
				ent.Project = p.Name
			}
		}
		if m != nil && m.Author != nil {
			ensureSessionOwner(ent, m.Author.ID, m.Author.String())
		}
	}); err != nil {
		return err
	} else if ok {
		return nil
	}
	// No session yet: create a shell.
	e := sessionstore.Entry{Goal: goal}
	if s != nil {
		parentID := parentChannelID(s, threadID)
		if p, err := b.resolveProject(parentID); err == nil {
			e.Project = p.Name
		}
	}
	if m != nil && m.Author != nil {
		ensureSessionOwner(&e, m.Author.ID, m.Author.String())
	}
	return b.sessions.Set(threadID, e)
}

// ensureThreadGoal sets sticky Goal from the first real task prompt when empty.
func (b *Bot) ensureThreadGoal(threadID, prompt string) {
	if b == nil || b.sessions == nil || threadID == "" {
		return
	}
	goal := clampGoal(prompt)
	if goal == "" {
		return
	}
	if _, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if strings.TrimSpace(ent.Goal) == "" {
			ent.Goal = goal
		}
	}); err != nil {
		log.Printf("warn: set goal thread=%s: %v", threadID, err)
	} else if !ok {
		// No session yet — refreshBriefCard may create a shell with history-derived goal.
	}
}

// refreshBriefCard builds and upserts the continuity message for a thread.
// cwd is optional; when empty, session Cwd / MainCwd is used for git.
// Returns whether the message was successfully pinned.
func (b *Bot) refreshBriefCard(s *discordgo.Session, threadID, cwd string) (pinned bool, err error) {
	if s == nil || threadID == "" {
		return false, fmt.Errorf("missing session or thread")
	}

	e, _ := b.sessions.Get(threadID)
	in := b.collectBriefInput(threadID, e, cwd)
	card := FormatBriefCard(in)
	if card == "" {
		return false, fmt.Errorf("empty brief card")
	}

	oldBriefMsgID := e.BriefMsgID
	msgID, err := b.upsertBriefMessage(s, threadID, oldBriefMsgID, card)
	if err != nil {
		return false, err
	}

	// Persist BriefMsgID (and sticky goal when derived from history).
	if b.sessions != nil {
		stickyGoal := clampGoal(in.Goal)
		if _, ok, pErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			ent.BriefMsgID = msgID
			if ent.Goal == "" && stickyGoal != "" {
				ent.Goal = stickyGoal
			}
		}); pErr != nil {
			log.Printf("brief: save msg id thread=%s: %v", threadID, pErr)
		} else if !ok {
			// No session yet: create a shell so the pin survives.
			shell := sessionstore.Entry{
				Project:    in.Project,
				Goal:       stickyGoal,
				BriefMsgID: msgID,
				OwnerID:    e.OwnerID,
				OwnerName:  e.OwnerName,
				CoOwnerIDs: append([]string(nil), e.CoOwnerIDs...),
			}
			if err := b.sessions.Set(threadID, shell); err != nil {
				log.Printf("brief: create session shell thread=%s: %v", threadID, err)
			}
		}
	}

	if pinErr := s.ChannelMessagePin(threadID, msgID); pinErr != nil {
		// Pin needs Pin Messages (split from Manage Messages); card still works unpinned.
		log.Printf("brief: pin thread=%s msg=%s: %v", threadID, msgID, pinErr)
		return false, nil
	}
	// When the card was re-posted (edit of the old message failed), drop the
	// previous pin so only the current brief stays pinned.
	if oldBriefMsgID != "" && oldBriefMsgID != msgID {
		if unpinErr := s.ChannelMessageUnpin(threadID, oldBriefMsgID); unpinErr != nil {
			log.Printf("brief: unpin old thread=%s msg=%s: %v", threadID, oldBriefMsgID, unpinErr)
		}
	}
	return true, nil
}

func (b *Bot) upsertBriefMessage(s *discordgo.Session, threadID, msgID, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return msgID, fmt.Errorf("empty card content")
	}
	if msgID != "" {
		if err := discordEdit(s, threadID, msgID, content); err == nil {
			return msgID, nil
		} else {
			log.Printf("brief: edit card %s: %v — posting new", msgID, err)
		}
	}
	msg, err := discordSend(s, threadID, content)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (b *Bot) collectBriefInput(threadID string, e sessionstore.Entry, cwd string) BriefCardInput {
	in := BriefCardInput{
		Project:   e.Project,
		OwnerID:   e.OwnerID,
		OwnerName: e.OwnerName,
		Goal:      strings.TrimSpace(e.Goal),
		Label:     sessionstore.DisplayLabel(e.EffectiveLabel()),
		Branch:    e.WorktreeBranch,
		Queue:     b.queueLen(threadID),
	}
	if e.LabelManual {
		in.LabelMode = "manual"
	}

	state := "idle"
	if job, busy := b.getJob(threadID); busy {
		state = "running · " + formatElapsed(time.Since(job.start))
	}
	if in.Queue > 0 {
		state += fmt.Sprintf(" · %d queued", in.Queue)
	}
	in.Status = state

	in.IssueLines = sessionstore.FormatIssueStatusLines(e.Issues)

	e.NormalizePRs()
	in.PRLines = ghpr.FormatMultiStatusLines(b.discordPRInfos(e))

	var lastResponse string
	if b.history != nil && threadID != "" {
		if th, err := b.history.Get(threadID); err == nil {
			in.Turns = len(th.Turns)
			in.Done = briefDoneFromHistory(th)
			if in.Goal == "" {
				in.Goal = briefGoalFromHistory(th)
			}
			lastResponse = lastAssistantResponse(th)
		}
	}

	// Resolve cwd for git snapshot.
	if cwd == "" {
		cwd = e.Cwd
	}
	if cwd == "" {
		cwd = e.MainCwd
	}
	if cwd != "" && gitworktree.IsRepo(cwd) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		diff, err := CollectDiffSummary(ctx, cwd, b.riskyPathGlobs())
		cancel()
		if err == nil {
			if in.Branch == "" {
				in.Branch = diff.Branch
			}
			in.HeadShort = diff.HeadShort
			in.Files = diff.NameStatus
		}
	}

	in.Left = briefLeft(in, e, lastResponse)
	in.Questions = extractOpenQuestions(lastResponse, maxBriefQuestions)
	return in
}

func briefDoneFromHistory(th history.Thread) []string {
	if len(th.Turns) == 0 {
		return nil
	}
	// Walk newest-first, keep non-cancelled turns with a prompt.
	var out []string
	for i := len(th.Turns) - 1; i >= 0 && len(out) < maxBriefDoneTurns; i-- {
		t := th.Turns[i]
		p := strings.TrimSpace(t.Prompt)
		if p == "" {
			continue
		}
		st := strings.TrimSpace(t.Status)
		if st == "" {
			st = "done"
		}
		if st == "cancelled" {
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s)", truncateRunes(p, maxBriefPreviewRunes), st))
	}
	// Reverse to chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func briefGoalFromHistory(th history.Thread) string {
	for _, t := range th.Turns {
		p := clampGoal(t.Prompt)
		if p != "" {
			return p
		}
	}
	return ""
}

func lastAssistantResponse(th history.Thread) string {
	for i := len(th.Turns) - 1; i >= 0; i-- {
		if r := strings.TrimSpace(th.Turns[i].Response); r != "" {
			return r
		}
	}
	return ""
}

func briefLeft(in BriefCardInput, e sessionstore.Entry, _ string) string {
	var parts []string
	if strings.HasPrefix(in.Status, "running") {
		parts = append(parts, "run in progress")
	}
	if in.Queue > 0 {
		parts = append(parts, fmt.Sprintf("%d follow-up%s queued", in.Queue, plural(in.Queue)))
	}

	e.NormalizePRs()
	if e.HasOpenPR() {
		// Prefer review/CI signals from primary PR.
		if p, ok := e.PrimaryPR(); ok {
			checks := strings.ToLower(p.Checks)
			review := strings.ToUpper(p.Review)
			switch {
			case strings.Contains(checks, "✗") || strings.Contains(checks, "fail"):
				parts = append(parts, "CI failing — `@Grok /fix-ci`")
			case review == "CHANGES_REQUESTED":
				parts = append(parts, "changes requested on PR")
			case p.IsDraft:
				parts = append(parts, "draft PR open")
			default:
				parts = append(parts, "PR open — review/merge")
			}
		} else {
			parts = append(parts, "PR open")
		}
	}

	// Dirty worktree without open PR still means leftover work.
	if len(in.Files) > 0 && !e.HasOpenPR() && !strings.HasPrefix(in.Status, "running") {
		// Heuristic: any "?" status = uncommitted.
		dirty := false
		for _, f := range in.Files {
			if strings.HasPrefix(f, "?\t") {
				dirty = true
				break
			}
		}
		if dirty {
			parts = append(parts, "uncommitted changes")
		}
	}

	if len(parts) == 0 {
		if in.Turns == 0 {
			return "no work yet — `@Grok <task>`"
		}
		return "ready for next `@Grok` task"
	}
	return strings.Join(parts, " · ")
}

// extractOpenQuestions pulls short question-like lines from assistant text.
func extractOpenQuestions(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" || max <= 0 {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "*-• \t")
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, "?") {
			continue
		}
		// Skip very long or non-sentence noise.
		if len([]rune(line)) > 160 || len([]rune(line)) < 8 {
			continue
		}
		// Prefer lines that look like questions (letter before ?).
		r := []rune(line)
		if !unicode.IsLetter(r[0]) && r[0] != '"' && r[0] != '\'' {
			continue
		}
		q := truncateRunes(line, maxBriefPreviewRunes)
		key := questionDedupeKey(q)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		// Also skip when an existing question is a suffix/prefix of this one.
		dup := false
		for k := range seen {
			if strings.Contains(key, k) || strings.Contains(k, key) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, q)
		if len(out) >= max {
			break
		}
	}
	return out
}

func questionDedupeKey(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	q = strings.TrimRight(q, "?")
	q = strings.TrimSpace(q)
	// Prefer the clause after the last colon/semicolon for "And again: Should we…".
	if i := strings.LastIndexAny(q, ":;"); i >= 0 && i+1 < len(q) {
		rest := strings.TrimSpace(q[i+1:])
		if len(rest) >= 8 {
			q = rest
		}
	}
	return q
}

// preserveBriefFields copies continuity card fields when session Set overwrites the entry.
func preserveBriefFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if next.Goal == "" {
		next.Goal = prev.Goal
	}
	if next.BriefMsgID == "" {
		next.BriefMsgID = prev.BriefMsgID
	}
}
