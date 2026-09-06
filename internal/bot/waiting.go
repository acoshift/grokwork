package bot

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Waiting reasons, ranked (lowest SortRank first).
const (
	WaitingReasonSLA      WaitingReason = "sla"
	WaitingReasonDecision WaitingReason = "decision"
	WaitingReasonCI       WaitingReason = "ci"
	WaitingReasonReview   WaitingReason = "review"
)

const (
	waitingKindSession = "session"
	waitingKindCase    = "case"
	waitingKindReview  = "review"
)

// waitingCap is how many rows Today may return. Matched is the pre-cap total.
const waitingCap = 40

const waitingUntitled = "(untitled)"

// WaitingReason is one chip on a Today row.
type WaitingReason string

// WaitingQuery is one personal queue request.
//
// Among copies CaseBoardQuery.Among: nil is unrestricted (admin / tests);
// a non-nil list (including empty) is the ACL set. A named Project that is
// not in Among yields an empty board.
type WaitingQuery struct {
	ActorID   string
	LookupIDs []string
	Project   string
	Among     []string
}

// WaitingRow is one unit or team-review request that still needs the viewer.
type WaitingRow struct {
	Kind         string
	ThreadID     string
	Project      string
	Title        string
	CaseKey      string
	URL          string
	Reasons      []WaitingReason
	SortRank     int
	UpdatedAt    string
	DecisionText string
	PRNumber     int
	PRURL        string
	Owner        string
	GHOwner      string
	GHRepo       string
}

// WaitingBoard is the personal queue: Rows honor the cap; Matched is the
// pre-cap count so a nav pill is not a silently truncated list.
type WaitingBoard struct {
	Rows    []WaitingRow
	Matched int
	Shown   int
	Project string
	Cap     int
}

// ListWaitingOnYou is the live join for /today.
func (b *Bot) ListWaitingOnYou(q WaitingQuery) WaitingBoard {
	return b.ListWaitingOnYouAt(q, time.Now())
}

// ListWaitingOnYouAt is ListWaitingOnYou with a frozen clock so SLA tests
// do not sleep. CaseSLAFor uses time.Now() and is the wrong helper here.
func (b *Bot) ListWaitingOnYouAt(q WaitingQuery, now time.Time) WaitingBoard {
	board := WaitingBoard{
		Project: strings.TrimSpace(q.Project),
		Cap:     waitingCap,
		Rows:    []WaitingRow{},
	}
	actorID := strings.TrimSpace(q.ActorID)
	if actorID == "" || b == nil || b.sessions == nil {
		return board
	}
	lookup := waitingLookupIDs(actorID, q.LookupIDs)

	var allowed map[string]struct{}
	if q.Among != nil {
		allowed = make(map[string]struct{}, len(q.Among))
		for _, n := range q.Among {
			n = strings.TrimSpace(n)
			if n != "" {
				allowed[n] = struct{}{}
			}
		}
		if board.Project != "" {
			if _, ok := allowed[board.Project]; !ok {
				return board
			}
		}
	}

	var rows []WaitingRow
	for _, listed := range b.sessions.List() {
		e := listed.Entry
		if !waitingProjectOK(e.Project, board.Project, allowed) {
			continue
		}
		if skipWaitingEntry(e) {
			continue
		}
		if !unitInvolves(e, actorID) {
			continue
		}
		row, ok := b.waitingRowFromEntry(listed.ThreadID, e, now)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}

	if revs := b.Reviews(); revs != nil {
		seenPR := map[string]struct{}{}
		for _, req := range revs.ListForReviewerAny(lookup, board.Project, reviewstore.StatusPending) {
			if !waitingProjectOK(req.Project, board.Project, allowed) {
				continue
			}
			key := reviewstore.PRKey(req.Owner, req.Repo, req.Number)
			if _, dup := seenPR[key]; dup {
				continue
			}
			seenPR[key] = struct{}{}
			rows = append(rows, b.waitingRowFromReview(req))
		}
	}

	slices.SortFunc(rows, cmpWaitingRow)
	board.Matched = len(rows)
	if len(rows) > waitingCap {
		rows = rows[:waitingCap]
	}
	board.Rows = rows
	board.Shown = len(rows)
	return board
}

func waitingLookupIDs(actorID string, extra []string) []string {
	ids := make([]string, 0, 1+len(extra))
	ids = append(ids, actorID)
	for _, id := range extra {
		id = strings.TrimSpace(id)
		if id == "" || id == actorID {
			continue
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func waitingProjectOK(project, filter string, allowed map[string]struct{}) bool {
	project = strings.TrimSpace(project)
	if filter != "" && !strings.EqualFold(project, filter) {
		return false
	}
	if allowed == nil {
		return true
	}
	_, ok := allowed[project]
	return ok
}

func skipWaitingEntry(e sessionstore.Entry) bool {
	if e.IsPRAsk() {
		return true
	}
	if e.IsCase() {
		return e.IsCaseClosed()
	}
	return sessionstore.IsTerminalLabel(e.EffectiveLabel())
}

// unitInvolves reports whether actorID is on this unit for Today.
// Watchers count here; they must not count in caseIsMine.
func unitInvolves(e sessionstore.Entry, actorID string) bool {
	if actorID == "" {
		return false
	}
	if e.OwnerID == actorID || e.EngineerID == actorID {
		return true
	}
	return slices.Contains(e.CoOwnerIDs, actorID) || slices.Contains(e.WatcherIDs, actorID)
}

func questionIsOpen(q sessionstore.OpenQuestion) bool {
	switch strings.ToLower(strings.TrimSpace(q.Status)) {
	case "answered", "dismissed":
		return false
	default:
		return true
	}
}

func (b *Bot) waitingRowFromEntry(threadID string, e sessionstore.Entry, now time.Time) (WaitingRow, bool) {
	var reasons []WaitingReason
	decisionText := ""
	if e.IsCase() && b.caseSLAAt(e, now).Breached {
		reasons = append(reasons, WaitingReasonSLA)
	}
	for _, q := range e.OpenQuestions {
		if !questionIsOpen(q) {
			continue
		}
		if decisionText == "" {
			decisionText = truncateRunes(strings.TrimSpace(q.Text), 200)
		}
		if !slices.Contains(reasons, WaitingReasonDecision) {
			reasons = append(reasons, WaitingReasonDecision)
		}
	}
	e.NormalizePRs()
	var failPR sessionstore.TrackedPR
	for _, pr := range e.PRs {
		if ghpr.IsTerminal(pr.State) || !checksLookFailing(pr.Checks) {
			continue
		}
		reasons = append(reasons, WaitingReasonCI)
		failPR = pr
		break
	}
	if len(reasons) == 0 {
		return WaitingRow{}, false
	}
	kind := waitingKindSession
	if e.IsCase() {
		kind = waitingKindCase
	}
	row := WaitingRow{
		Kind:         kind,
		ThreadID:     threadID,
		Project:      e.Project,
		Title:        waitingTitle(e),
		CaseKey:      strings.TrimSpace(e.CaseKey),
		URL:          inboxSessionPath(threadID, e.Project),
		Reasons:      reasons,
		SortRank:     waitingRank(reasons[0]),
		UpdatedAt:    e.UpdatedAt,
		DecisionText: decisionText,
		Owner:        cmp.Or(strings.TrimSpace(e.OwnerName), e.OwnerID),
	}
	if failPR.Number > 0 || failPR.URL != "" {
		row.PRNumber = failPR.Number
		row.PRURL = cmp.Or(strings.TrimSpace(failPR.URL), inboxPRPath(failPR.Owner, failPR.Repo, failPR.Number, e.Project))
		row.GHOwner = failPR.Owner
		row.GHRepo = failPR.Repo
	}
	return row, true
}

func (b *Bot) waitingRowFromReview(req reviewstore.Request) WaitingRow {
	title := fmt.Sprintf("%s/%s#%d", req.Owner, req.Repo, req.Number)
	if req.ThreadID != "" && b.sessions != nil {
		if e, ok := b.sessions.Get(req.ThreadID); ok &&
			strings.EqualFold(strings.TrimSpace(e.Project), strings.TrimSpace(req.Project)) {
			if t := waitingTitle(e); t != waitingUntitled {
				title = t
			}
		}
	}
	updated := ""
	if !req.CreatedAt.IsZero() {
		updated = req.CreatedAt.UTC().Format(time.RFC3339)
	}
	prURL := inboxPRPath(req.Owner, req.Repo, req.Number, req.Project)
	return WaitingRow{
		Kind:      waitingKindReview,
		ThreadID:  req.ThreadID,
		Project:   req.Project,
		Title:     title,
		URL:       prURL,
		Reasons:   []WaitingReason{WaitingReasonReview},
		SortRank:  waitingRank(WaitingReasonReview),
		UpdatedAt: updated,
		PRNumber:  req.Number,
		PRURL:     prURL,
		Owner:     cmp.Or(strings.TrimSpace(req.ReviewerName), req.ReviewerID),
		GHOwner:   req.Owner,
		GHRepo:    req.Repo,
	}
}

func waitingTitle(e sessionstore.Entry) string {
	if e.IsCase() {
		if t := strings.TrimSpace(e.CustomerTitle); t != "" {
			return t
		}
	}
	if t := strings.TrimSpace(e.Goal); t != "" {
		return t
	}
	return waitingUntitled
}

func waitingRank(r WaitingReason) int {
	switch r {
	case WaitingReasonSLA:
		return 0
	case WaitingReasonDecision:
		return 1
	case WaitingReasonCI:
		return 2
	case WaitingReasonReview:
		return 3
	default:
		return 9
	}
}

func cmpWaitingRow(a, b WaitingRow) int {
	if n := cmp.Compare(a.SortRank, b.SortRank); n != 0 {
		return n
	}
	if n := cmp.Compare(b.UpdatedAt, a.UpdatedAt); n != 0 {
		return n
	}
	return cmp.Compare(a.ThreadID, b.ThreadID)
}
