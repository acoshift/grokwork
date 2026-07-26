package bot

import (
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// CaseRow is one Mode=case session on the web case board.
type CaseRow struct {
	ThreadID string
	Project  string
	// CaseKey is the case's quotable id ("WEBAPP-14"); empty on cases filed
	// before keys existed.
	CaseKey     string
	Phase       string // normalized; unknown/empty phases bucket as intake
	Severity    string // low|medium|high|critical (normalized) or ""
	Title       string // CustomerTitle → Goal → "(untitled case)"
	CustomerRef string
	OwnerID     string
	OwnerName   string
	// EngineerName is the assigned engineer; NeedsEngineer is the board's
	// waiting-for-engineering flag, computed once (needsEngineer) so the row chip and
	// the owner filter cannot disagree about what "unassigned" means.
	EngineerName  string
	NeedsEngineer bool
	ReporterName  string
	Origin        string
	DiscordURL    string
	Running       bool
	Queue         int
	UpdatedAt     string

	CustomerUpdate string // latest support-facing update (already clamped)
	DossierSummary string // internal investigation summary
	Resolution     string // answered|fixed|duplicate|wontfix|escalated_external

	// Primary tracked PR (escalated cases in fixing/shipping).
	PRNumber        int
	PRState         string // display state: OPEN, DRAFT, MERGED, CLOSED
	PRChecks        string
	PRChecksFailing bool
	PRURL           string
	GHOwner         string
	GHRepo          string
}

// CaseGroup is one phase lane of filtered rows, in pipeline order.
type CaseGroup struct {
	Phase string
	Plain string // support-facing phrasing (CasePhasePlain)
	Rows  []CaseRow
}

// CaseBoard is the support case pipeline for the web UI (K3: cases are
// Mode=case sessions grouped by Phase, never by Label alone).
type CaseBoard struct {
	ProjectFilter  string
	PhaseFilter    string
	SeverityFilter string
	Scope          string // "open" (default: hide closed) | "all"
	OwnerFilter    string // "" | mine | unassigned

	Groups []CaseGroup
	Shown  int // rows after phase/severity/scope/owner filters

	// Pipeline counts over the project's cases (pre phase/severity/scope filters).
	Intake      int
	Investigate int
	Answered    int
	Fixing      int
	Shipping    int
	Closed      int
	OpenTotal   int
	Total       int
	// Mine / Unassigned label the owner filter's options, so unlike the phase counts
	// above they are counted *after* the other filters — the number has to equal the
	// rows selecting it would show. They overlap each other: an unassigned case its
	// reporter owns is both.
	Mine       int
	Unassigned int
}

// CasePhaseOrder is pipeline display order (open stages, then closed).
var CasePhaseOrder = []string{
	sessionstore.PhaseIntake,
	sessionstore.PhaseInvestigate,
	sessionstore.PhaseAnswered,
	sessionstore.PhaseFixing,
	sessionstore.PhaseShipping,
	sessionstore.PhaseClosed,
}

// CasePhasePlain maps a phase to the plain-language projection shown to
// support alongside the technical phase name (design doc "Plain-language status").
func CasePhasePlain(phase string) string {
	switch phase {
	case sessionstore.PhaseIntake:
		return "New case"
	case sessionstore.PhaseInvestigate:
		return "Looking into it"
	case sessionstore.PhaseAnswered:
		return "Answer ready"
	case sessionstore.PhaseFixing:
		return "With engineering"
	case sessionstore.PhaseShipping:
		return "Fix in review"
	case sessionstore.PhaseClosed:
		return "Resolved"
	default:
		return ""
	}
}

// normalizeCasePhase buckets unknown/empty phases as intake so no case can
// fall off the board through a bad phase value.
func normalizeCasePhase(e sessionstore.Entry) string {
	p := e.CasePhase()
	if slices.Contains(CasePhaseOrder, p) {
		return p
	}
	return sessionstore.PhaseIntake
}

// needsEngineer reports whether a case is waiting for an engineer to pick it up.
//
// Only the engineering phases qualify. "No engineer assigned" is true of every
// intake and investigate case too, but those are support-side phases where no
// engineer is expected — counting them made the filter select the entire board and
// disagreed with the per-row chip, which is drawn on the same condition.
func needsEngineer(e sessionstore.Entry, phase string) bool {
	if e.EngineerID != "" {
		return false
	}
	return phase == sessionstore.PhaseFixing || phase == sessionstore.PhaseShipping
}

// caseIsMine reports whether userID is on this case: the assigned engineer, or the
// thread owner / co-owner. Owners count so "mine" is useful to the support member
// who filed the case, not only to engineers — nobody has to learn which of the two
// fields their role happens to land in.
func caseIsMine(e sessionstore.Entry, userID string) bool {
	if userID == "" {
		return false
	}
	if e.EngineerID == userID || e.OwnerID == userID {
		return true
	}
	return slices.Contains(e.CoOwnerIDs, userID)
}

func caseSeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

// Case owner filter values (CaseBoardQuery.Owner).
const (
	// CaseOwnerMine is "cases I am on": the engineer assignment, or thread
	// ownership so a support member still finds the cases they filed.
	CaseOwnerMine = "mine"
	// CaseOwnerUnassigned is the engineering triage queue: no engineer yet.
	CaseOwnerUnassigned = "unassigned"
)

// CaseBoardQuery is one case-board request. Grouped into a struct because the
// filters travel together from the query string and kept growing as positional
// arguments.
type CaseBoardQuery struct {
	Project  string
	Phase    string
	Severity string
	Scope    string // "open" (default: hide closed) | "all"
	// Owner is "" (everyone), CaseOwnerMine, or CaseOwnerUnassigned.
	Owner string
	// ViewerID is who "mine" means. An empty id makes "mine" match nothing rather
	// than matching every unassigned case.
	ViewerID string
	// Among restricts to these project names (nil = unrestricted). The web layer
	// passes a member's visible projects so the global board never leaks cases from
	// projects they cannot open.
	Among []string
}

// ListCaseBoard collects Mode=case sessions grouped by phase. projectFilter
// empty means all projects (SSE fingerprinting and the global cases board).
// Pipeline counts cover the project-filtered set; Groups honor
// phase/severity/scope. scope "" or "open" hides closed unless the phase
// filter explicitly asks for closed.
func (b *Bot) ListCaseBoard(projectFilter, phaseFilter, severityFilter, scope string) CaseBoard {
	return b.ListCaseBoardQuery(CaseBoardQuery{
		Project: projectFilter, Phase: phaseFilter, Severity: severityFilter, Scope: scope,
	})
}

// ListCaseBoardAmong is ListCaseBoard restricted to the given project names.
func (b *Bot) ListCaseBoardAmong(projectFilter, phaseFilter, severityFilter, scope string, among []string) CaseBoard {
	return b.ListCaseBoardQuery(CaseBoardQuery{
		Project: projectFilter, Phase: phaseFilter, Severity: severityFilter, Scope: scope,
		Among: among,
	})
}

// ListCaseBoardQuery is the full board query.
func (b *Bot) ListCaseBoardQuery(q CaseBoardQuery) CaseBoard {
	phaseFilter := strings.ToLower(strings.TrimSpace(q.Phase))
	severityFilter := strings.ToLower(strings.TrimSpace(q.Severity))
	scope := strings.ToLower(strings.TrimSpace(q.Scope))
	if scope != "all" {
		scope = "open"
	}
	ownerFilter := strings.ToLower(strings.TrimSpace(q.Owner))
	if ownerFilter != CaseOwnerMine && ownerFilter != CaseOwnerUnassigned {
		ownerFilter = ""
	}
	viewerID := strings.TrimSpace(q.ViewerID)
	board := CaseBoard{
		ProjectFilter:  strings.TrimSpace(q.Project),
		PhaseFilter:    phaseFilter,
		SeverityFilter: severityFilter,
		Scope:          scope,
		OwnerFilter:    ownerFilter,
	}
	if b == nil || b.sessions == nil {
		return board
	}

	var allowed map[string]struct{}
	if q.Among != nil {
		allowed = make(map[string]struct{}, len(q.Among))
		for _, n := range q.Among {
			n = strings.TrimSpace(n)
			if n != "" {
				allowed[n] = struct{}{}
			}
		}
	}

	var rows []CaseRow
	for _, listed := range b.sessions.List() {
		e := listed.Entry
		if !e.IsCase() {
			continue
		}
		if board.ProjectFilter != "" && !strings.EqualFold(e.Project, board.ProjectFilter) {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[e.Project]; !ok {
				continue
			}
		}
		phase := normalizeCasePhase(e)
		switch phase {
		case sessionstore.PhaseIntake:
			board.Intake++
		case sessionstore.PhaseInvestigate:
			board.Investigate++
		case sessionstore.PhaseAnswered:
			board.Answered++
		case sessionstore.PhaseFixing:
			board.Fixing++
		case sessionstore.PhaseShipping:
			board.Shipping++
		case sessionstore.PhaseClosed:
			board.Closed++
		}
		if severityFilter != "" && strings.ToLower(strings.TrimSpace(e.Severity)) != severityFilter {
			continue
		}
		if phaseFilter != "" && phase != phaseFilter {
			continue
		}
		if phaseFilter == "" && scope != "all" && phase == sessionstore.PhaseClosed {
			continue
		}
		// Counted here — after every filter except the owner one — so "Mine (3)" is
		// exactly what selecting Mine will show. Counting earlier (like the phase
		// counters, which are deliberately project-wide) made the labels drift from
		// the rows as the closed archive grew. The two overlap rather than partition:
		// a case I filed that nobody has picked up is both.
		if needsEngineer(e, phase) {
			board.Unassigned++
		}
		if viewerID != "" && caseIsMine(e, viewerID) {
			board.Mine++
		}
		switch ownerFilter {
		case CaseOwnerMine:
			if viewerID == "" || !caseIsMine(e, viewerID) {
				continue
			}
		case CaseOwnerUnassigned:
			if !needsEngineer(e, phase) {
				continue
			}
		}
		rows = append(rows, b.caseRowFrom(listed.ThreadID, e, phase))
	}
	board.OpenTotal = board.Intake + board.Investigate + board.Answered + board.Fixing + board.Shipping
	board.Total = board.OpenTotal + board.Closed

	sortCaseRows(rows)
	for _, ph := range CasePhaseOrder {
		var group []CaseRow
		for _, r := range rows {
			if r.Phase == ph {
				group = append(group, r)
			}
		}
		if len(group) > 0 {
			board.Groups = append(board.Groups, CaseGroup{Phase: ph, Plain: CasePhasePlain(ph), Rows: group})
		}
	}
	board.Shown = len(rows)
	return board
}

func (b *Bot) caseRowFrom(threadID string, e sessionstore.Entry, phase string) CaseRow {
	title := caseRowTitle(e)
	row := CaseRow{
		ThreadID:       threadID,
		Project:        e.Project,
		CaseKey:        strings.TrimSpace(e.CaseKey),
		Phase:          phase,
		Severity:       strings.ToLower(strings.TrimSpace(e.Severity)),
		Title:          title,
		CustomerRef:    strings.TrimSpace(e.CustomerRef),
		OwnerID:        e.OwnerID,
		OwnerName:      e.OwnerName,
		EngineerName:   strings.TrimSpace(e.EngineerName),
		NeedsEngineer:  needsEngineer(e, phase),
		ReporterName:   strings.TrimSpace(e.ReporterName),
		Origin:         strings.TrimSpace(e.Origin),
		DiscordURL:     strings.TrimSpace(e.DiscordURL),
		Running:        b.isThreadBusy(threadID),
		Queue:          b.queueLen(threadID),
		UpdatedAt:      e.UpdatedAt,
		CustomerUpdate: strings.TrimSpace(e.CustomerUpdate),
		Resolution:     strings.ToLower(strings.TrimSpace(e.Resolution)),
	}
	if e.Dossier != nil {
		row.DossierSummary = strings.TrimSpace(e.Dossier.Summary)
	}
	if pr, ok := e.PrimaryPR(); ok {
		pr.FillOwnerRepoFromURL()
		row.PRNumber = pr.Number
		row.PRState = ghpr.DisplayState(ghpr.Info{
			Number: pr.Number, URL: pr.URL, State: pr.State, IsDraft: pr.IsDraft,
		})
		row.PRChecks = strings.TrimSpace(pr.Checks)
		row.PRChecksFailing = checksLookFailing(pr.Checks)
		row.PRURL = pr.URL
		row.GHOwner = pr.Owner
		row.GHRepo = pr.Repo
	}
	return row
}

// sortCaseRows orders triage-first: severity (critical → low → unset), then
// newest session activity, then thread id for stability.
func sortCaseRows(rows []CaseRow) {
	slices.SortStableFunc(rows, func(a, b CaseRow) int {
		if ra, rb := caseSeverityRank(a.Severity), caseSeverityRank(b.Severity); ra != rb {
			return ra - rb
		}
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
