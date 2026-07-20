package bot

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// ShipPRRow is one tracked PR for the ship board web page.
type ShipPRRow struct {
	ThreadID    string
	Project     string
	OwnerID     string
	OwnerName   string
	Goal        string
	Label       string
	LabelManual bool
	Running     bool
	Queue       int
	UpdatedAt   string

	URL      string
	Number   int
	State    string // OPEN, DRAFT, MERGED, CLOSED (display)
	RawState string // OPEN, MERGED, CLOSED from gh/session
	Title    string
	Checks   string
	Review   string
	HeadRef  string
	IsDraft  bool
	GHOwner  string
	GHRepo   string

	ChecksFailing    bool
	ChangesRequested bool
	ReviewApproved   bool
}

// ShipBoard is a lead-facing view of all bot-tracked PRs.
type ShipBoard struct {
	Rows          []ShipPRRow
	ProjectFilter string
	StateFilter   string // open | all | draft | merged | closed | failing
	Projects      []string

	Open             int
	Draft            int
	ChecksFailing    int
	ChangesRequested int
	Approved         int
	Merged           int
	Closed           int
	Total            int

	// Digest is a plain-text lead summary of open PRs (copyable).
	Digest string
}

// ListShipBoard collects tracked PRs from sessions. projectFilter and stateFilter
// are optional (empty project = all; empty state defaults to "open").
// Stats and the lead digest always cover the project-filtered set; Rows honor stateFilter.
func (b *Bot) ListShipBoard(projectFilter, stateFilter string) ShipBoard {
	projectFilter = strings.TrimSpace(projectFilter)
	stateFilter = strings.ToLower(strings.TrimSpace(stateFilter))
	if stateFilter == "" {
		stateFilter = "open"
	}

	board := ShipBoard{
		ProjectFilter: projectFilter,
		StateFilter:   stateFilter,
		Rows:          make([]ShipPRRow, 0),
	}
	if b == nil {
		board.Digest = formatShipDigest(board, nil)
		return board
	}
	if b.cfg != nil {
		board.Projects = b.cfg.ProjectNames()
	}
	if b.sessions == nil {
		board.Digest = formatShipDigest(board, nil)
		return board
	}

	list := b.sessions.List()
	all := make([]ShipPRRow, 0)
	for _, listed := range list {
		e := listed.Entry
		e.NormalizePRs()
		if len(e.PRs) == 0 {
			continue
		}
		if projectFilter != "" && !strings.EqualFold(e.Project, projectFilter) {
			continue
		}
		goal := strings.TrimSpace(e.Goal)
		if goal == "" {
			goal = b.lastPromptPreview(listed.ThreadID)
		}
		running := b.isThreadBusy(listed.ThreadID)
		qlen := b.queueLen(listed.ThreadID)
		for _, pr := range e.PRs {
			row := shipRowFrom(listed.ThreadID, e, pr, goal, running, qlen)
			all = append(all, row)
		}
	}

	// Counts always reflect project-filtered set before state filter (open focus for leads).
	for _, r := range all {
		board.Total++
		switch r.State {
		case "DRAFT":
			board.Draft++
			board.Open++ // drafts are open for shipping attention
		case "OPEN":
			board.Open++
		case "MERGED":
			board.Merged++
		case "CLOSED":
			board.Closed++
		}
		if r.ChecksFailing && !ghpr.IsTerminal(r.RawState) {
			board.ChecksFailing++
		}
		if r.ChangesRequested && !ghpr.IsTerminal(r.RawState) {
			board.ChangesRequested++
		}
		if r.ReviewApproved && !ghpr.IsTerminal(r.RawState) {
			board.Approved++
		}
	}

	for _, r := range all {
		if shipStateMatch(r, stateFilter) {
			board.Rows = append(board.Rows, r)
		}
	}
	sortShipRows(board.Rows)

	// Digest always lists open PRs for the project (not the table state filter).
	openForDigest := make([]ShipPRRow, 0)
	for _, r := range all {
		if !ghpr.IsTerminal(r.RawState) {
			openForDigest = append(openForDigest, r)
		}
	}
	sortShipRows(openForDigest)
	board.Digest = formatShipDigest(board, openForDigest)
	return board
}

func shipRowFrom(threadID string, e sessionstore.Entry, pr sessionstore.TrackedPR, goal string, running bool, queue int) ShipPRRow {
	pr.FillOwnerRepoFromURL()
	info := ghpr.Info{
		Number:         pr.Number,
		URL:            pr.URL,
		Title:          pr.Title,
		State:          pr.State,
		IsDraft:        pr.IsDraft,
		ReviewDecision: pr.Review,
		HeadRef:        pr.HeadRef,
		Checks:         pr.Checks,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
	}
	display := ghpr.DisplayState(info)
	review := strings.ToUpper(strings.TrimSpace(pr.Review))
	row := ShipPRRow{
		ThreadID:         threadID,
		Project:          e.Project,
		OwnerID:          e.OwnerID,
		OwnerName:        e.OwnerName,
		Goal:             goal,
		Label:            e.EffectiveLabel(),
		LabelManual:      e.LabelManual,
		Running:          running,
		Queue:            queue,
		UpdatedAt:        e.UpdatedAt,
		URL:              pr.URL,
		Number:           pr.Number,
		State:            display,
		RawState:         strings.ToUpper(strings.TrimSpace(pr.State)),
		Title:            strings.TrimSpace(pr.Title),
		Checks:           strings.TrimSpace(pr.Checks),
		Review:           strings.TrimSpace(pr.Review),
		HeadRef:          pr.HeadRef,
		IsDraft:          pr.IsDraft,
		GHOwner:          pr.Owner,
		GHRepo:           pr.Repo,
		ChecksFailing:    checksLookFailing(pr.Checks),
		ChangesRequested: review == "CHANGES_REQUESTED",
		ReviewApproved:   review == "APPROVED",
	}
	if row.RawState == "" && !ghpr.IsTerminal(display) {
		row.RawState = "OPEN"
	}
	return row
}

func checksLookFailing(checks string) bool {
	return strings.Contains(checks, "✗")
}

func shipStateMatch(r ShipPRRow, filter string) bool {
	switch filter {
	case "all":
		return true
	case "open":
		return !ghpr.IsTerminal(r.RawState)
	case "draft":
		return r.State == "DRAFT" || (r.IsDraft && !ghpr.IsTerminal(r.RawState))
	case "merged":
		return r.State == "MERGED" || strings.EqualFold(r.RawState, "MERGED")
	case "closed":
		return r.State == "CLOSED" || strings.EqualFold(r.RawState, "CLOSED")
	case "failing":
		return r.ChecksFailing && !ghpr.IsTerminal(r.RawState)
	default:
		// Unknown filter: treat as open so the page stays useful.
		return !ghpr.IsTerminal(r.RawState)
	}
}

func sortShipRows(rows []ShipPRRow) {
	rank := func(r ShipPRRow) int {
		// Attention first: CI fail → changes requested → open → draft → terminal.
		if !ghpr.IsTerminal(r.RawState) {
			if r.ChecksFailing {
				return 0
			}
			if r.ChangesRequested {
				return 1
			}
			if r.State == "OPEN" {
				return 2
			}
			if r.State == "DRAFT" {
				return 3
			}
			return 4
		}
		if r.State == "MERGED" {
			return 10
		}
		return 11
	}
	slices.SortFunc(rows, func(a, b ShipPRRow) int {
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra - rb
		}
		// Newest session activity first within a bucket.
		switch {
		case a.UpdatedAt == b.UpdatedAt:
			if a.ThreadID != b.ThreadID {
				return strings.Compare(a.ThreadID, b.ThreadID)
			}
			return a.Number - b.Number
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

// formatShipDigest builds a plain-text lead summary. openRows should be non-terminal
// PRs for the project filter (independent of the table's state filter).
func formatShipDigest(board ShipBoard, openRows []ShipPRRow) string {
	var b strings.Builder
	scope := "all projects"
	if board.ProjectFilter != "" {
		scope = board.ProjectFilter
	}
	fmt.Fprintf(&b, "Ship board · %s · %s\n", scope, time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "Open: %d · Drafts: %d · CI failing: %d · Changes requested: %d · Approved: %d",
		board.Open, board.Draft, board.ChecksFailing, board.ChangesRequested, board.Approved)
	if board.Merged+board.Closed > 0 {
		fmt.Fprintf(&b, " · Merged: %d · Closed: %d", board.Merged, board.Closed)
	}
	b.WriteByte('\n')

	if len(openRows) == 0 {
		b.WriteString("\n(no open bot PRs)\n")
		return b.String()
	}
	b.WriteByte('\n')
	for _, r := range openRows {
		label := prShortLabel(r)
		title := r.Title
		if title == "" {
			title = r.Goal
		}
		if title == "" {
			title = "(no title)"
		}
		title = truncateRunes(title, 90)
		line := fmt.Sprintf("• %s · %s — %s", label, r.State, title)
		if r.Checks != "" {
			line += " · checks " + r.Checks
		}
		if r.Review != "" {
			line += " · review " + humanReviewShort(r.Review)
		}
		if r.Project != "" {
			line += " · " + r.Project
		}
		owner := r.OwnerName
		if owner == "" && r.OwnerID != "" {
			owner = r.OwnerID
		}
		if owner != "" {
			line += " · @" + owner
		}
		line += " · thread " + r.ThreadID
		if r.URL != "" {
			line += "\n  " + r.URL
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func prShortLabel(r ShipPRRow) string {
	if r.GHOwner != "" && r.GHRepo != "" && r.Number > 0 {
		return fmt.Sprintf("%s/%s#%d", r.GHOwner, r.GHRepo, r.Number)
	}
	if r.Number > 0 {
		return fmt.Sprintf("#%d", r.Number)
	}
	if r.URL != "" {
		return r.URL
	}
	return "PR?"
}

func humanReviewShort(r string) string {
	switch strings.ToUpper(strings.TrimSpace(r)) {
	case "APPROVED":
		return "APPROVED"
	case "CHANGES_REQUESTED":
		return "CHANGES_REQUESTED"
	case "REVIEW_REQUIRED":
		return "REVIEW_REQUIRED"
	default:
		return r
	}
}
