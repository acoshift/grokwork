package web

import (
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
)

func (s *Server) todayPage(ctx *hime.Context) error {
	d := s.todayPageData(ctx, strings.TrimSpace(ctx.FormValue("project")), false)
	return s.viewPage(ctx, "today", d)
}

func (s *Server) todayScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.todayPageData(ctx, project, true)
	return s.viewPage(ctx, "today", d)
}

func (s *Server) partialTodayList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	scoped := ctx.FormValue("scoped") == "1"
	if scoped {
		if err := s.ensureProjectAccess(ctx, project); err != nil {
			return forbiddenProject(ctx, err)
		}
	}
	return s.viewFragment(ctx, "today", "today_list", s.todayPageData(ctx, project, scoped))
}

func (s *Server) todayPageData(ctx *hime.Context, project string, scoped bool) pageData {
	d := s.basePage(ctx)
	d.IsToday = true
	d.Waiting = s.bot.ListWaitingOnYou(s.waitingQuery(ctx, project))
	d.InboxUnread = s.inboxUnreadVisible(ctx)
	if scoped {
		d.Project = project
		d.Title = project + " · Today"
	} else {
		d.Title = "Today"
	}
	return d
}

// waitingQuery builds the personal queue request. project is a data filter
// (global ?project=) or a workspace scope. Unauthorized names are ignored
// rather than 403ing the whole page — callers 403 workspace paths themselves.
func (s *Server) waitingQuery(ctx *hime.Context, project string) bot.WaitingQuery {
	actor := s.fixActor(ctx)
	q := bot.WaitingQuery{ActorID: actor.ID}
	if actor.ID != "" {
		q.LookupIDs = s.reviewerLookupIDs(actor.ID)
	}
	_, role := s.sessionIdentity(ctx)
	if !config.RoleAtLeast(role, config.WebRoleAdmin) {
		q.Among = s.filterProjectNames(ctx)
	}
	project = strings.TrimSpace(project)
	if project != "" && s.ensureProjectAccess(ctx, project) == nil {
		q.Project = project
	}
	return q
}

func (s *Server) waitingMatched(ctx *hime.Context, project string) int {
	return s.bot.ListWaitingOnYou(s.waitingQuery(ctx, project)).Matched
}
