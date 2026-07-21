package bot

import (
	"slices"
	"strings"

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
}

// ListShipBoard collects tracked PRs from sessions. projectFilter and stateFilter
// are optional (empty project = all; empty state defaults to "open").
// Stats always cover the project-filtered set; Rows honor stateFilter.
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
		return board
	}
	if b.cfg != nil {
		board.Projects = b.cfg.ProjectNames()
	}
	if b.sessions == nil {
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
	// Stable keys only — do not sort by session UpdatedAt. The PR poller
	// patches every open session each cycle, so UpdatedAt thrashing made
	// the table reshuffle on every SSE reload.
	slices.SortStableFunc(rows, func(a, b ShipPRRow) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		if c := strings.Compare(strings.ToLower(a.Project), strings.ToLower(b.Project)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.GHOwner), strings.ToLower(b.GHOwner)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.GHRepo), strings.ToLower(b.GHRepo)); c != 0 {
			return c
		}
		// Higher PR number first within a repo (usually newer).
		if a.Number != b.Number {
			return b.Number - a.Number
		}
		return strings.Compare(a.ThreadID, b.ThreadID)
	})
}
