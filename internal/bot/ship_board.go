package bot

import (
	"slices"
	"strconv"
	"strings"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/reviewstore"
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
	Review   string // GitHub reviewDecision
	HeadRef  string
	HeadSHA  string
	IsDraft  bool
	GHOwner  string
	GHRepo   string

	ChecksFailing      bool
	ChangesRequested   bool // true if GH CR or team sticky CR (attention sort)
	ReviewApproved     bool // GH APPROVED
	GHChangesRequested bool // GitHub reviewDecision only
	GHReviewApproved   bool // GitHub reviewDecision only

	// Team review (local reviewstore; Discord-attributed).
	TeamRollup        string // reviewstore.Rollup*
	TeamPending       int
	TeamReviewSummary string // short badge text for the table

	// FromCase is true when the session Mode=case (support-originated ship path).
	FromCase  bool
	CasePhase string // case phase when FromCase
	CaseTitle string // CustomerTitle when set

	// SessionCount is how many work units bind this PR. Always ≥1; >1 means the
	// row is a merge and its session-side fields name only the primary unit.
	SessionCount int

	// Imported is true when the primary unit is an import shell (no grokwork run yet).
	Imported bool
}

// ShipBoard is a lead-facing view of all bot-tracked PRs.
type ShipBoard struct {
	Rows          []ShipPRRow
	ProjectFilter string
	StateFilter   string // open | all | draft | merged | closed | failing | needs_team_review | team_approved | team_changes
	Projects      []string

	Open             int
	Draft            int
	ChecksFailing    int
	ChangesRequested int
	Approved         int
	TeamAwaiting     int // open PRs with team rollup review_requested or changes_requested
	Merged           int
	Closed           int
	Total            int
}

// ListShipBoard collects tracked PRs from sessions. projectFilter and stateFilter
// are optional (empty project = all; empty state defaults to "open").
// Stats always cover the project-filtered set; Rows honor stateFilter.
func (b *Bot) ListShipBoard(projectFilter, stateFilter string) ShipBoard {
	return b.ListShipBoardAmong(projectFilter, stateFilter, nil)
}

// ListShipBoardAmong is ListShipBoard restricted to project names in among.
// among nil means unrestricted (all configured projects). Empty among yields an
// empty board with an empty Projects dropdown (for web ACL filtering).
func (b *Bot) ListShipBoardAmong(projectFilter, stateFilter string, among []string) ShipBoard {
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

	var allowed map[string]struct{}
	if among != nil {
		allowed = make(map[string]struct{}, len(among))
		for _, n := range among {
			n = strings.TrimSpace(n)
			if n != "" {
				allowed[n] = struct{}{}
			}
		}
		// Preserve caller order for the filter dropdown.
		board.Projects = append([]string(nil), among...)
		if projectFilter != "" {
			if _, ok := allowed[projectFilter]; !ok {
				return board
			}
		}
	} else if b.cfg != nil {
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
		if allowed != nil {
			if _, ok := allowed[e.Project]; !ok {
				continue
			}
		}
		goal := strings.TrimSpace(e.Goal)
		if goal == "" {
			goal = b.lastPromptPreview(listed.ThreadID)
		}
		running := b.isThreadBusy(listed.ThreadID)
		qlen := b.queueLen(listed.ThreadID)
		for _, pr := range e.PRs {
			row := shipRowFrom(listed.ThreadID, e, pr, goal, running, qlen)
			b.enrichTeamReview(&row)
			all = append(all, row)
		}
	}

	// One row per PR, not per (session, PR) pair — see mergeShipRows. Done before
	// counting so "N tracked" counts pull requests, like the rows it describes.
	all = mergeShipRows(all)

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
		if !ghpr.IsTerminal(r.RawState) &&
			(r.TeamRollup == reviewstore.RollupReviewRequested ||
				r.TeamRollup == reviewstore.RollupChangesRequested) {
			board.TeamAwaiting++
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
		ThreadID:           threadID,
		Project:            e.Project,
		OwnerID:            e.OwnerID,
		OwnerName:          e.OwnerName,
		Goal:               goal,
		Label:              e.EffectiveLabel(),
		LabelManual:        e.LabelManual,
		Running:            running,
		Queue:              queue,
		UpdatedAt:          e.UpdatedAt,
		FromCase:           e.IsCase(),
		CasePhase:          e.CasePhase(),
		CaseTitle:          strings.TrimSpace(e.CustomerTitle),
		URL:                pr.URL,
		Number:             pr.Number,
		State:              display,
		RawState:           strings.ToUpper(strings.TrimSpace(pr.State)),
		Title:              strings.TrimSpace(pr.Title),
		Checks:             strings.TrimSpace(pr.Checks),
		Review:             strings.TrimSpace(pr.Review),
		HeadRef:            pr.HeadRef,
		HeadSHA:            pr.HeadSHA,
		IsDraft:            pr.IsDraft,
		GHOwner:            pr.Owner,
		GHRepo:             pr.Repo,
		ChecksFailing:      checksLookFailing(pr.Checks),
		ChangesRequested:   review == "CHANGES_REQUESTED",
		ReviewApproved:     review == "APPROVED",
		GHChangesRequested: review == "CHANGES_REQUESTED",
		GHReviewApproved:   review == "APPROVED",
		Imported:           e.IsImportedPR(),
	}
	if row.RawState == "" && !ghpr.IsTerminal(display) {
		row.RawState = "OPEN"
	}
	return row
}

func checksLookFailing(checks string) bool {
	return strings.Contains(checks, "✗")
}

// mergeShipRows collapses rows describing the same pull request.
//
// Rows are built per (session, tracked PR) pair, so a PR that two units bind —
// an Address CI session and a review session, say — produced two near-identical
// rows on a board that is about pull requests, not sessions.
//
// The merge splits along the two kinds of fact a row carries:
//
//   - PR facts (state, checks, review, title, head, and everything derived from
//     them) come from the unit holding the freshest copy. pollPRStatuses skips
//     busy threads, so a unit with a run in flight holds the STALE copy — idle
//     units win, newest first.
//   - Session facts (which unit to open, its owner and goal) come from the unit
//     a human would want: running, then queued, then newest.
//
// Those two are often different units, so the rest aggregates: Running/Queue so
// an in-flight run still shows even when the PR facts came from an idle unit,
// Label as the most attention-needing of the group, UpdatedAt as the latest
// movement, and SessionCount so the row admits how many units are behind it.
func mergeShipRows(rows []ShipPRRow) []ShipPRRow {
	if len(rows) == 0 {
		return rows
	}
	order := make([]string, 0, len(rows))
	groups := make(map[string][]ShipPRRow, len(rows))
	for _, r := range rows {
		k := shipRowKey(r)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	out := make([]ShipPRRow, 0, len(order))
	for _, k := range order {
		out = append(out, mergeShipGroup(groups[k]))
	}
	return out
}

// shipRowKey identifies the PR a row is about. A row whose PR cannot be
// identified at all merges with nothing — guessing would fuse unrelated PRs.
func shipRowKey(r ShipPRRow) string {
	pr := sessionstore.TrackedPR{URL: r.URL, Number: r.Number, Owner: r.GHOwner, Repo: r.GHRepo}
	key := pr.PRKey()
	if key == "" {
		return "unit\x00" + r.ThreadID
	}
	return strings.ToLower(strings.TrimSpace(r.Project)) + "\x00" + key
}

func mergeShipGroup(group []ShipPRRow) ShipPRRow {
	if len(group) == 1 {
		row := group[0]
		row.SessionCount = 1
		return row
	}
	fresh, primary := group[0], group[0]
	for _, r := range group[1:] {
		if fresherPRRecord(r, fresh) {
			fresh = r
		}
		if livelierSession(r, primary) {
			primary = r
		}
	}
	// Start from the freshest PR record so every PR-derived flag (ChecksFailing,
	// ChangesRequested, the team rollup) stays consistent with the checks and
	// review text it was computed from.
	row := fresh
	row.ThreadID = primary.ThreadID
	row.OwnerID, row.OwnerName, row.Goal = primary.OwnerID, primary.OwnerName, primary.Goal
	row.Imported = primary.Imported
	// Distinct units, not rows: one entry listing the same PR twice is one
	// session, and the badge says "sessions".
	units := make(map[string]struct{}, len(group))
	for _, r := range group {
		units[r.ThreadID] = struct{}{}
	}
	row.SessionCount = len(units)
	row.Running, row.Queue = false, 0
	row.FromCase, row.CasePhase, row.CaseTitle = false, "", ""
	row.Label, row.LabelManual = "", false
	for _, r := range group {
		row.Running = row.Running || r.Running
		row.Queue = max(row.Queue, r.Queue)
		if r.UpdatedAt > row.UpdatedAt {
			row.UpdatedAt = r.UpdatedAt
		}
		if r.FromCase && !row.FromCase {
			row.FromCase, row.CasePhase, row.CaseTitle = true, r.CasePhase, r.CaseTitle
		}
		if row.Label == "" || labelAttention(r.Label) < labelAttention(row.Label) {
			row.Label, row.LabelManual = r.Label, r.LabelManual
		}
	}
	return row
}

// fresherPRRecord reports whether a holds a more current PR snapshot than b.
// The poller skips busy threads, so "not running" beats "running" outright.
func fresherPRRecord(a, b ShipPRRow) bool {
	if a.Running != b.Running {
		return !a.Running
	}
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt > b.UpdatedAt
	}
	return a.ThreadID < b.ThreadID
}

// livelierSession reports whether a is the unit a reader would rather open.
//
// Deliberately does NOT fall back to UpdatedAt, for the reason sortShipRows
// gives: the poller patches every open session each cycle, so between two
// equally idle units recency is a coin flip that lands differently every tick.
// This picks the row's Owner and Goal, so an unstable answer would make those
// cells swap on every SSE refresh. Thread id is arbitrary but it holds still.
func livelierSession(a, b ShipPRRow) bool {
	if a.Running != b.Running {
		return a.Running
	}
	if (a.Queue > 0) != (b.Queue > 0) {
		return a.Queue > 0
	}
	if at, bt := sessionstore.IsTerminalLabel(a.Label), sessionstore.IsTerminalLabel(b.Label); at != bt {
		return !at
	}
	if a.Imported != b.Imported {
		return !a.Imported
	}
	return a.ThreadID < b.ThreadID
}

// labelAttention ranks a session label by how much it wants a human, reusing
// the board display order (blocked first … abandoned last). Unknown labels sort
// last so they never outrank a real one.
func labelAttention(label string) int {
	if i := slices.Index(sessionstore.CanonicalLabels, label); i >= 0 {
		return i
	}
	return len(sessionstore.CanonicalLabels)
}

func (b *Bot) enrichTeamReview(row *ShipPRRow) {
	if b == nil || b.reviews == nil || row == nil || row.Number <= 0 {
		return
	}
	bucket := b.reviews.ListForPR(row.GHOwner, row.GHRepo, row.Number)
	label, pending, effectives := reviewstore.TeamRollup(bucket, row.HeadSHA)
	row.TeamRollup = label
	row.TeamPending = pending
	row.TeamReviewSummary = formatTeamReviewSummary(label, pending, effectives)
	if label == reviewstore.RollupChangesRequested {
		row.ChangesRequested = true
	}
}

func formatTeamReviewSummary(label string, pending int, effectives []reviewstore.EffectiveReview) string {
	parts := make([]string, 0, len(effectives)+1)
	for _, er := range effectives {
		name := strings.TrimSpace(er.ReviewerName)
		if name == "" {
			name = er.ReviewerID
		}
		if len(name) > 16 {
			name = name[:16]
		}
		switch er.Verdict {
		case reviewstore.VerdictApproved:
			if er.Stale {
				parts = append(parts, name+" ⏳")
			} else {
				parts = append(parts, name+" ✅")
			}
		case reviewstore.VerdictChangesRequested:
			if er.Stale {
				parts = append(parts, name+" 🔄·stale")
			} else {
				parts = append(parts, name+" 🔄")
			}
		}
	}
	switch label {
	case reviewstore.RollupReviewRequested:
		if pending > 0 {
			parts = append(parts, "+"+strconv.Itoa(pending)+" pending")
		}
	case reviewstore.RollupNone:
		if len(parts) == 0 {
			return "—"
		}
	case reviewstore.RollupStaleApprovals:
		if len(parts) == 0 {
			return "stale"
		}
	}
	if len(parts) == 0 {
		if pending > 0 {
			return "+" + strconv.Itoa(pending) + " pending"
		}
		return "—"
	}
	return strings.Join(parts, " · ")
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
	case "needs_team_review":
		return !ghpr.IsTerminal(r.RawState) &&
			(r.TeamRollup == reviewstore.RollupReviewRequested || r.TeamRollup == reviewstore.RollupNone ||
				r.TeamRollup == reviewstore.RollupStaleApprovals)
	case "team_approved":
		return !ghpr.IsTerminal(r.RawState) && r.TeamRollup == reviewstore.RollupApproved
	case "team_changes":
		return !ghpr.IsTerminal(r.RawState) && r.TeamRollup == reviewstore.RollupChangesRequested
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
	// Stable keys only — do not sort by session UpdatedAt. Turn stamps move
	// rows for real work; PR/check noise must not reshuffle the table on SSE.
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
