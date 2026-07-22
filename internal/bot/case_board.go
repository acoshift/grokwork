package bot

import (
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// CaseRow is one Mode=case session on the web case board.
type CaseRow struct {
	ThreadID     string
	Project      string
	Phase        string // normalized; unknown/empty phases bucket as intake
	Severity     string // low|medium|high|critical (normalized) or ""
	Title        string // CustomerTitle → Goal → "(untitled case)"
	CustomerRef  string
	OwnerID      string
	OwnerName    string
	ReporterName string
	Origin       string
	DiscordURL   string
	Running      bool
	Queue        int
	UpdatedAt    string

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

	Groups []CaseGroup
	Shown  int // rows after phase/severity/scope filters

	// Pipeline counts over the project's cases (pre phase/severity/scope filters).
	Intake      int
	Investigate int
	Answered    int
	Fixing      int
	Shipping    int
	Closed      int
	OpenTotal   int
	Total       int
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

// ListCaseBoard collects Mode=case sessions grouped by phase. projectFilter
// empty means all projects (used by SSE fingerprinting; web pages are always
// project-scoped). Pipeline counts cover the project-filtered set; Groups
// honor phase/severity/scope. scope "" or "open" hides closed unless the
// phase filter explicitly asks for closed.
func (b *Bot) ListCaseBoard(projectFilter, phaseFilter, severityFilter, scope string) CaseBoard {
	phaseFilter = strings.ToLower(strings.TrimSpace(phaseFilter))
	severityFilter = strings.ToLower(strings.TrimSpace(severityFilter))
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != "all" {
		scope = "open"
	}
	board := CaseBoard{
		ProjectFilter:  strings.TrimSpace(projectFilter),
		PhaseFilter:    phaseFilter,
		SeverityFilter: severityFilter,
		Scope:          scope,
	}
	if b == nil || b.sessions == nil {
		return board
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
	title := strings.TrimSpace(e.CustomerTitle)
	if title == "" {
		title = strings.TrimSpace(e.Goal)
	}
	if title == "" {
		title = "(untitled case)"
	}
	row := CaseRow{
		ThreadID:       threadID,
		Project:        e.Project,
		Phase:          phase,
		Severity:       strings.ToLower(strings.TrimSpace(e.Severity)),
		Title:          title,
		CustomerRef:    strings.TrimSpace(e.CustomerRef),
		OwnerID:        e.OwnerID,
		OwnerName:      e.OwnerName,
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
