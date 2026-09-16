package web

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
)

// todaySessionCap is how many of the viewer's active sessions Today may
// return. TodaySessionMatched is the pre-cap total so the page can say so.
const todaySessionCap = 40

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
	sessions, matched := s.listTodaySessions(ctx, d.Waiting.Project)
	d.TodaySessions = sessions
	d.TodaySessionMatched = matched
	d.TodaySessionShown = len(sessions)
	if scoped {
		d.Project = project
		d.Title = project + " · Today"
	} else {
		d.Title = "Today"
	}
	return d
}

// listTodaySessions is /sessions?owner=mine&state=active for the Today page:
// visibility first, then the same mine + active predicates, then a cap.
// Empty actor matches nothing (auth off / unsigned). History list errors
// degrade to an empty section so the waiting queue still renders.
func (s *Server) listTodaySessions(ctx *hime.Context, project string) ([]history.Summary, int) {
	actorID := strings.TrimSpace(s.fixActor(ctx).ID)
	if actorID == "" || s.history == nil || s.sessions == nil {
		return nil, 0
	}
	threads, err := s.history.List()
	if err != nil {
		return nil, 0
	}
	threads = mergeSessionRows(threads, s.sessions.List())
	threads = s.filterThreadsVisible(ctx, threads)
	threads = dropPRAskRows(threads)
	annotateSessionRunning(threads, s.bot)
	return clipTodaySessions(threads, sessionFilters{
		State:    "active",
		Owner:    sessionOwnerMine,
		ViewerID: actorID,
		Project:  project,
	}, time.Now())
}

// clipTodaySessions applies the sessions-list mine/active filter, sorts live
// runs first then newest UpdatedAt, and caps the page.
func clipTodaySessions(threads []history.Summary, f sessionFilters, now time.Time) ([]history.Summary, int) {
	rows := filterSessionRows(threads, f, now)
	slices.SortFunc(rows, cmpTodaySession)
	matched := len(rows)
	if len(rows) > todaySessionCap {
		rows = rows[:todaySessionCap]
	}
	return rows, matched
}

func cmpTodaySession(a, b history.Summary) int {
	if a.Running != b.Running {
		if a.Running {
			return -1
		}
		return 1
	}
	if n := cmp.Compare(b.UpdatedAt, a.UpdatedAt); n != 0 {
		return n
	}
	return cmp.Compare(a.ThreadID, b.ThreadID)
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
