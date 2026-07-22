package web

import (
	"strings"

	"github.com/moonrhythm/hime"
)

// casesScoped is the workspace support case board: Mode=case sessions for one
// project, grouped by lifecycle phase (K3). Always project-scoped — there is
// no cross-project cases hub; access mirrors the other workspace pages.
func (s *Server) casesScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.casesPageData(ctx, project)
	d.Title = project + " · Cases"
	return s.viewPage(ctx, "cases", d)
}

// casesPageData builds the board from the current filter query. Partials pass
// the same query with ?project= so SSE refreshes keep the client's filters.
func (s *Server) casesPageData(ctx *hime.Context, project string) pageData {
	d := s.basePage(ctx)
	d.IsCases = true
	d.Project = project
	d.Cases = s.bot.ListCaseBoard(project,
		ctx.FormValue("phase"), ctx.FormValue("severity"), ctx.FormValue("scope"))
	return d
}

func (s *Server) partialCasesPipeline(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "cases", "cases_pipeline", s.casesPageData(ctx, project))
}

func (s *Server) partialCasesList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "cases", "cases_list", s.casesPageData(ctx, project))
}
