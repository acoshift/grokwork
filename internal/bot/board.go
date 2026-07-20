package bot

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const (
	maxBoardMsgRunes = 1800
	maxBoardRows     = 40
)

// Activity buckets for the team board (mutually exclusive, priority order).
const (
	activityRunning   = "running"
	activityQueued    = "queued"
	activityWaiting   = "waiting"
	activityStale     = "stale"
	activityActive    = "active"
	activityDone      = "done"
	activityAbandoned = "abandoned"
)

// CanonicalActivityOrder is display order for board sections.
var canonicalActivityOrder = []string{
	activityRunning,
	activityQueued,
	activityWaiting,
	activityStale,
	activityActive,
	activityDone,
	activityAbandoned,
}

func (b *Bot) handleBoard(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	// Board is always scoped to this channel's mapped project (same as tasks).
	parentID := parentChannelID(s, m.ChannelID)
	proj, err := b.resolveProject(parentID)
	if err != nil {
		if _, sendErr := s.ChannelMessageSendReply(m.ChannelID, err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply board-resolve: %v", sendErr)
		}
		return
	}

	labelFilter, activityFilter, includeTerminal, errMsg := parseBoardArgs(parsed.Prompt)
	if errMsg != "" {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, errMsg, ref(m)); err != nil {
			log.Printf("error: reply board-usage: %v", err)
		}
		return
	}

	projectFilter := proj.Name
	staleDays := b.boardStaleDays()
	rows := b.collectBoardRows(projectFilter, labelFilter, activityFilter, includeTerminal, staleDays, time.Now())
	body := formatBoardCard(rows, projectFilter, labelFilter, activityFilter, includeTerminal, staleDays)
	if _, err := s.ChannelMessageSendReply(m.ChannelID, body, ref(m)); err != nil {
		log.Printf("error: reply board: %v", err)
	}
}

func (b *Bot) boardStaleDays() int {
	if b == nil || b.cfg == nil {
		return config.DefaultBoardStaleDays
	}
	return b.cfg.BoardStaleDaysValue()
}

// parseBoardArgs: /board [label|activity|all]
// label uses ParseLabel; activity is running|queued|waiting|stale|active;
// "all" includes terminal labels. Project scope comes from the channel mapping.
func parseBoardArgs(prompt string) (label, activity string, includeTerminal bool, errMsg string) {
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
		if act, ok := parseActivityFilter(f); ok {
			activity = act
			if act == activityDone || act == activityAbandoned {
				includeTerminal = true
			}
			continue
		}
		if lab, ok := sessionstore.ParseLabel(f); ok {
			label = lab
			if sessionstore.IsTerminalLabel(lab) {
				includeTerminal = true
			}
			continue
		}
		return "", "", false, fmt.Sprintf(
			"Unknown board filter `%s`. Use `@Grok /board [running|queued|waiting|stale|active|label|all]`.\n"+
				"Scoped to this channel's project. Activity: running, queued, waiting (on human), stale, active · Labels: open, in_progress, blocked, needs_review, done, abandoned.",
			f,
		)
	}
	return label, activity, includeTerminal, ""
}

func parseActivityFilter(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	switch s {
	case activityRunning, "run":
		return activityRunning, true
	case activityQueued, "queue":
		return activityQueued, true
	case activityWaiting, "waiting_on_human", "wait", "human", "attention":
		return activityWaiting, true
	case activityStale, "idle":
		return activityStale, true
	case activityActive, "other":
		return activityActive, true
	case activityDone:
		return activityDone, true
	case activityAbandoned:
		return activityAbandoned, true
	default:
		return "", false
	}
}

func displayActivity(bucket string) string {
	switch bucket {
	case activityRunning:
		return "running"
	case activityQueued:
		return "queued"
	case activityWaiting:
		return "waiting on human"
	case activityStale:
		return "stale"
	case activityActive:
		return "active"
	case activityDone:
		return "done"
	case activityAbandoned:
		return "abandoned"
	default:
		return bucket
	}
}

type boardRow struct {
	ThreadID   string
	Project    string
	Label      string
	Goal       string
	OwnerID    string
	Running    bool
	Queue      int
	UpdatedAt  string
	Activity   string
	WaitReason string
	IdleDays   int // whole days since UpdatedAt; 0 if unknown/recent
}

func (b *Bot) collectBoardRows(projectFilter, labelFilter, activityFilter string, includeTerminal bool, staleDays int, now time.Time) []boardRow {
	if b == nil || b.sessions == nil {
		return nil
	}
	if staleDays <= 0 {
		staleDays = config.DefaultBoardStaleDays
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
		running := false
		if _, busy := b.getJob(listed.ThreadID); busy {
			running = true
		}
		qlen := b.queueLen(listed.ThreadID)
		waitReason := waitingOnHumanReason(e)
		idleDays := idleWholeDays(e.UpdatedAt, now)
		act := classifyActivity(running, qlen, waitReason, lab, idleDays, staleDays)
		if activityFilter != "" && act != activityFilter {
			continue
		}
		out = append(out, boardRow{
			ThreadID:   listed.ThreadID,
			Project:    e.Project,
			Label:      lab,
			Goal:       goal,
			OwnerID:    e.OwnerID,
			Running:    running,
			Queue:      qlen,
			UpdatedAt:  e.UpdatedAt,
			Activity:   act,
			WaitReason: waitReason,
			IdleDays:   idleDays,
		})
	}
	// Prefer attention buckets over merely-newest sessions when truncating.
	sortBoardRows(out)
	if len(out) > maxBoardRows {
		out = out[:maxBoardRows]
	}
	return out
}

func activityRank(act string) int {
	for i, a := range canonicalActivityOrder {
		if a == act {
			return i
		}
	}
	return len(canonicalActivityOrder)
}

func sortBoardRows(rows []boardRow) {
	slices.SortStableFunc(rows, func(a, b boardRow) int {
		if ra, rb := activityRank(a.Activity), activityRank(b.Activity); ra != rb {
			return ra - rb
		}
		// Within a bucket, newest session activity first (UpdatedAt RFC3339 sorts lexicographically).
		switch {
		case a.UpdatedAt == b.UpdatedAt:
			return strings.Compare(a.ThreadID, b.ThreadID)
		case a.UpdatedAt == "":
			return 1
		case b.UpdatedAt == "":
			return -1
		case a.UpdatedAt > b.UpdatedAt:
			return -1
		default:
			return 1
		}
	})
}

// classifyActivity assigns a mutually exclusive activity bucket.
// Priority: running → queued → waiting on human → stale → active (or terminal).
func classifyActivity(running bool, queue int, waitReason, label string, idleDays, staleDays int) string {
	if sessionstore.IsTerminalLabel(label) {
		if label == sessionstore.LabelAbandoned {
			return activityAbandoned
		}
		return activityDone
	}
	if running {
		return activityRunning
	}
	if queue > 0 {
		return activityQueued
	}
	if waitReason != "" {
		return activityWaiting
	}
	if idleDays >= staleDays {
		return activityStale
	}
	return activityActive
}

// waitingOnHumanReason returns a short reason when a human should act, else "".
func waitingOnHumanReason(e sessionstore.Entry) string {
	lab := e.EffectiveLabel()
	if lab == sessionstore.LabelBlocked {
		return "blocked"
	}
	e.NormalizePRs()
	for _, pr := range e.OpenPRs() {
		if strings.EqualFold(strings.TrimSpace(pr.Review), "CHANGES_REQUESTED") {
			return "changes requested"
		}
		if checksLookFailing(pr.Checks) {
			return "CI failing"
		}
	}
	if lab == sessionstore.LabelNeedsReview {
		return "needs review"
	}
	return ""
}

func idleWholeDays(updatedAt string, now time.Time) int {
	t := parseRFC3339(updatedAt)
	if t.IsZero() || now.IsZero() {
		return 0
	}
	d := now.Sub(t)
	if d < 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}

func formatBoardCard(rows []boardRow, projectFilter, labelFilter, activityFilter string, includeTerminal bool, staleDays int) string {
	var headParts []string
	headParts = append(headParts, "**Board**")
	if projectFilter != "" {
		headParts = append(headParts, "**"+projectFilter+"**")
	} else {
		headParts = append(headParts, "all projects")
	}
	switch {
	case activityFilter != "":
		headParts = append(headParts, displayActivity(activityFilter))
	case labelFilter != "":
		headParts = append(headParts, sessionstore.DisplayLabel(labelFilter))
	case includeTerminal:
		headParts = append(headParts, "including done/abandoned")
	default:
		headParts = append(headParts, "activity")
	}

	counts := countActivities(rows)
	queueFollowups := 0
	for _, r := range rows {
		queueFollowups += r.Queue
	}

	if len(rows) == 0 {
		return strings.Join(headParts, " · ") + "\n_(no matching threads)_"
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("%s · %d thread%s", strings.Join(headParts, " · "), len(rows), plural(len(rows))))
	// Compact stats line for the team snapshot.
	// "queued" bucket is rare today (follow-ups only exist while a job runs, and
	// those threads sit under running); still surface follow-up depth when present.
	if activityFilter == "" && labelFilter == "" {
		parts := []string{
			fmt.Sprintf("%d running", counts[activityRunning]),
		}
		if counts[activityQueued] > 0 {
			parts = append(parts, fmt.Sprintf("%d queued", counts[activityQueued]))
		}
		if queueFollowups > 0 {
			parts = append(parts, fmt.Sprintf("%d follow-up%s", queueFollowups, plural(queueFollowups)))
		}
		parts = append(parts,
			fmt.Sprintf("%d waiting", counts[activityWaiting]),
			fmt.Sprintf("%d stale", counts[activityStale]),
			fmt.Sprintf("%d active", counts[activityActive]),
		)
		if counts[activityDone]+counts[activityAbandoned] > 0 {
			parts = append(parts, fmt.Sprintf("%d terminal", counts[activityDone]+counts[activityAbandoned]))
		}
		lines = append(lines, strings.Join(parts, " · "))
	}

	byAct := map[string][]boardRow{}
	for _, r := range rows {
		byAct[r.Activity] = append(byAct[r.Activity], r)
	}

	for _, act := range canonicalActivityOrder {
		group := byAct[act]
		if len(group) == 0 {
			continue
		}
		title := displayActivity(act)
		if act == activityStale && staleDays > 0 {
			title = fmt.Sprintf("stale (≥%dd)", staleDays)
		}
		lines = append(lines, fmt.Sprintf("**%s** (%d)", title, len(group)))
		for _, r := range group {
			lines = append(lines, formatBoardLine(r))
		}
	}

	body := strings.Join(lines, "\n")
	return truncateRunes(body, maxBoardMsgRunes)
}

func countActivities(rows []boardRow) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.Activity]++
	}
	return out
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
	// Secondary signals (activity section already conveys primary bucket).
	switch r.Activity {
	case activityRunning:
		bits = append(bits, "_running_")
		if r.Queue > 0 {
			bits = append(bits, fmt.Sprintf("queue %d", r.Queue))
		}
	case activityQueued:
		bits = append(bits, fmt.Sprintf("queue %d", r.Queue))
	case activityWaiting:
		if r.WaitReason != "" {
			bits = append(bits, "_"+r.WaitReason+"_")
		} else {
			bits = append(bits, sessionstore.DisplayLabel(r.Label))
		}
	case activityStale:
		if r.IdleDays > 0 {
			bits = append(bits, fmt.Sprintf("idle %dd", r.IdleDays))
		}
		bits = append(bits, sessionstore.DisplayLabel(r.Label))
	default:
		if r.Label != "" && r.Label != sessionstore.LabelOpen {
			bits = append(bits, sessionstore.DisplayLabel(r.Label))
		}
		if r.Queue > 0 {
			bits = append(bits, fmt.Sprintf("queue %d", r.Queue))
		}
	}
	return "• " + strings.Join(bits, " · ")
}
