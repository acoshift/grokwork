package web

import (
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
)

// casesScoped is the workspace support case board: Mode=case sessions for one
// project, grouped by lifecycle phase (K3). Access mirrors the other
// workspace pages.
func (s *Server) casesScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.casesPageData(ctx, project)
	d.Title = project + " · Cases"
	return s.viewPage(ctx, "cases", d)
}

// casesGlobal is the cross-project case board (lead view, like /ship):
// membership-filtered per session; the shell stays global.
func (s *Server) casesGlobal(ctx *hime.Context) error {
	d := s.casesPageData(ctx, "")
	d.Title = "Cases"
	return s.viewPage(ctx, "cases", d)
}

// casesPageData builds the board from the current filter query. Partials pass
// the same query (project may be empty = global board) so SSE refreshes keep
// the client's filters.
func (s *Server) casesPageData(ctx *hime.Context, project string) pageData {
	d := s.basePage(ctx)
	d.IsCases = true
	d.Project = project
	if project != "" {
		d.CanOpenCase = s.canOpenCase(d, project)
	}
	d.Cases = s.listCaseBoardVisible(ctx, project)
	// Stamped onto every row's session link so the detail page's ← crumb
	// returns to this board with these filters still applied. Built from the
	// board's own resolved filters, not the raw query, so the SSE-refreshed
	// list partial and the first render agree.
	d.CasesBoardURL = casesBoardURL(project, d.Cases)
	return d
}

// casesBoardURL renders the current board as a root-relative URL. Only filters
// that actually narrow the board are carried: ListCaseBoardQuery resolves an
// absent scope to "open", so spelling that out would put a query string on
// every unfiltered board for no change in what it shows.
func casesBoardURL(project string, b bot.CaseBoard) string {
	base := caseBoardURL(project)
	q := url.Values{}
	for key, val := range map[string]string{
		"phase":    b.PhaseFilter,
		"severity": b.SeverityFilter,
		"owner":    b.OwnerFilter,
		"sla":      b.SLAFilter,
	} {
		if val != "" {
			q.Set(key, val)
		}
	}
	if b.Scope == "all" {
		q.Set("scope", "all")
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// listCaseBoardVisible restricts the global board to the session's visible
// projects (admins see everything); a named project filter passes through.
func (s *Server) listCaseBoardVisible(ctx *hime.Context, projectFilter string) bot.CaseBoard {
	_, role := s.sessionIdentity(ctx)
	q := bot.CaseBoardQuery{
		Project:  projectFilter,
		Phase:    ctx.FormValue("phase"),
		Severity: ctx.FormValue("severity"),
		Scope:    ctx.FormValue("scope"),
		Owner:    ctx.FormValue("owner"),
		SLA:      ctx.FormValue("sla"),
		ViewerID: s.fixActor(ctx).ID,
	}
	if !config.RoleAtLeast(role, config.WebRoleAdmin) {
		q.Among = s.filterProjectNames(ctx)
	}
	return s.bot.ListCaseBoardQuery(q)
}

// casesPartialData resolves a fragment request: a named project needs
// explicit access; empty project is the visibility-filtered global board.
func (s *Server) casesPartialData(ctx *hime.Context) (pageData, error) {
	project := strings.TrimSpace(ctx.FormValue("project"))
	if project != "" {
		if err := s.ensureProjectAccess(ctx, project); err != nil {
			return pageData{}, err
		}
	}
	return s.casesPageData(ctx, project), nil
}

func (s *Server) partialCasesPipeline(ctx *hime.Context) error {
	d, err := s.casesPartialData(ctx)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "cases", "cases_pipeline", d)
}

func (s *Server) partialCasesList(ctx *hime.Context) error {
	d, err := s.casesPartialData(ctx)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "cases", "cases_list", d)
}
