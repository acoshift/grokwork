package web

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/history"
)

// ── Project-first shell scope ────────────────────────────────────────────
//
// The sidebar renders in one of two modes: global (project launcher + lead
// views) or project workspace (scoped nav + switcher). The mode is derived
// from the URL alone — never from page data — so the layout JS can recompute
// it identically after an htmx history restore (the sidebar lives outside
// #live-root and is synced via hx-select-oob on boosted requests).

// navScopeFromURL derives the workspace project from the URL. Path wins:
// /projects/{p}/… and /config/projects/{p}. Detail pages whose canonical URL
// carries no project (/sessions/{id…}, /history/{id…}, /prs/…) may opt in via
// ?project=. Global list pages that use ?project= as a data filter (/ship)
// stay global. Mirror any change here in layout.tmpl's scopeFromLocation().
func navScopeFromURL(u *url.URL) string {
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(seg) >= 2 && seg[0] == "projects":
		return seg[1]
	case len(seg) >= 3 && seg[0] == "config" && seg[1] == "projects":
		return seg[2]
	case len(seg) >= 2 && (seg[0] == "sessions" || seg[0] == "history" || seg[0] == "prs"):
		return u.Query().Get("project")
	}
	return ""
}

// navScope validates the URL-derived scope: unknown or inaccessible projects
// fall back to the global shell (page handlers still enforce access).
func (s *Server) navScope(ctx *hime.Context) string {
	p := strings.TrimSpace(navScopeFromURL(ctx.Request.URL))
	if p == "" {
		return ""
	}
	if _, ok := s.cfg.ProjectPath(p); !ok {
		return ""
	}
	userID, role := s.sessionIdentity(ctx)
	if !s.cfg.CanAccessProject(p, userID, role) {
		return ""
	}
	return p
}

// ── Home: project launcher ───────────────────────────────────────────────

// projectCard is one project tile on the launcher.
type projectCard struct {
	Name          string
	Running       int
	Queued        int // follow-ups queued on running threads
	Sessions      int
	OpenPRs       int
	ChecksFailing int
	LastActivity  string // relative ("2h ago"); empty → no activity yet
}

// buildProjectCards aggregates live run, PR, and session activity per
// visible project for the launcher grid.
func (s *Server) buildProjectCards(ctx *hime.Context) []projectCard {
	names := s.filterProjectNames(ctx)
	if len(names) == 0 {
		return nil
	}
	idx := make(map[string]int, len(names))
	cards := make([]projectCard, len(names))
	for i, n := range names {
		cards[i] = projectCard{Name: n}
		idx[n] = i
	}
	for _, r := range s.bot.StatusSnapshot().ActiveRuns {
		if i, ok := idx[r.Project]; ok {
			cards[i].Running++
			cards[i].Queued += r.QueueLen
		}
	}
	for _, r := range s.bot.ListShipBoard("", "all").Rows {
		i, ok := idx[r.Project]
		if !ok {
			continue
		}
		switch r.State {
		case "OPEN", "DRAFT":
			cards[i].OpenPRs++
			if r.ChecksFailing {
				cards[i].ChecksFailing++
			}
		}
	}
	if threads, err := s.history.List(); err == nil {
		var lastAt = make([]string, len(names))
		for _, t := range mergeSessionRows(threads, s.sessions.List()) {
			if i, ok := idx[t.Project]; ok {
				cards[i].Sessions++
				if t.UpdatedAt > lastAt[i] {
					lastAt[i] = t.UpdatedAt
				}
			}
		}
		for i := range cards {
			cards[i].LastActivity = relativeAge(lastAt[i])
		}
	}
	return cards
}

// relativeAge renders an RFC3339 timestamp as a coarse age ("2h ago").
func relativeAge(rfc3339 string) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// shortTime formats a time.Time or RFC3339 string for table Date columns
// (same layout as commits: "2006-01-02 15:04").
func shortTime(v any) string {
	const layout = "2006-01-02 15:04"
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.Format(layout)
	case *time.Time:
		if x == nil || x.IsZero() {
			return ""
		}
		return x.Format(layout)
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return ""
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Format(layout)
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.Format(layout)
		}
		return s
	default:
		return ""
	}
}

// home is the project launcher: pick a project first, then work scoped.
func (s *Server) home(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Projects"
	d.IsDashboard = true
	d.ProjectCards = s.buildProjectCards(ctx)
	d.Status = s.statusVisible(ctx)
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "home", d)
}

func (s *Server) partialHomeProjects(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.ProjectCards = s.buildProjectCards(ctx)
	return s.viewFragment(ctx, "home", "home_projects", d)
}

func (s *Server) partialHomeRuns(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Status = s.statusVisible(ctx)
	return s.viewFragment(ctx, "home", "home_runs", d)
}

// redirectHome sends retired feature-first hubs (/issues, /commits) to the
// launcher — projects are picked first now.
func (s *Server) redirectHome(ctx *hime.Context) error {
	return ctx.RedirectTo("home")
}

// ── Project workspace: overview + scoped list pages ──────────────────────

// statusForProject narrows a status snapshot to one project's runs.
func statusForProject(snap bot.StatusSnapshot, project string) bot.StatusSnapshot {
	runs := make([]bot.ActiveRun, 0, len(snap.ActiveRuns))
	queued := 0
	for _, r := range snap.ActiveRuns {
		if r.Project == project {
			runs = append(runs, r)
			queued += r.QueueLen
		}
	}
	snap.ActiveRuns = runs
	snap.ActiveCount = len(runs)
	snap.QueuedTotal = queued
	return snap
}

func filterThreadsProject(threads []history.Summary, project string) []history.Summary {
	out := make([]history.Summary, 0, len(threads))
	for _, t := range threads {
		if t.Project == project {
			out = append(out, t)
		}
	}
	return out
}

func filterWorktreesProject(list []bot.WorktreeInfo, project string) []bot.WorktreeInfo {
	out := make([]bot.WorktreeInfo, 0, len(list))
	for _, w := range list {
		if w.Project == project {
			out = append(out, w)
		}
	}
	return out
}

// projectPulseData backs the overview's live region: scoped runs + open PRs.
func (s *Server) projectPulseData(ctx *hime.Context, project string) pageData {
	d := s.basePage(ctx)
	d.Project = project
	d.Status = statusForProject(s.bot.StatusSnapshot(), project)
	d.Ship = s.bot.ListShipBoard(project, "open")
	return d
}

// projectThreads returns the project's merged sessions, newest first.
// mergeSessionRows appends turn-less sessions after the sorted history
// list, so re-sort before slicing a "recent" prefix.
func (s *Server) projectThreads(project string) []history.Summary {
	threads, err := s.history.List()
	if err != nil {
		threads = nil
	}
	threads = filterThreadsProject(mergeSessionRows(threads, s.sessions.List()), project)
	sortThreadsByUpdated(threads)
	return threads
}

func sortThreadsByUpdated(threads []history.Summary) {
	// RFC3339 UTC sorts lexicographically; empty timestamps sink.
	for i := 1; i < len(threads); i++ {
		for j := i; j > 0; j-- {
			a, b := threads[j-1], threads[j]
			if a.UpdatedAt == "" && b.UpdatedAt != "" || (b.UpdatedAt != "" && a.UpdatedAt < b.UpdatedAt) {
				threads[j-1], threads[j] = b, a
			} else {
				break
			}
		}
	}
}

// projectOverview is the workspace landing page.
func (s *Server) projectOverview(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.projectPulseData(ctx, project)
	d.Title = project
	d.IsOverview = true
	threads := s.projectThreads(project)
	if len(threads) > 8 {
		threads = threads[:8]
	}
	d.Threads = threads
	return s.viewPage(ctx, "project_overview", d)
}

func (s *Server) partialProjectPulse(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "project_overview", "project_pulse", s.projectPulseData(ctx, project))
}

// shipScoped is the workspace ship board (state filter only; project fixed).
func (s *Server) shipScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	state := strings.TrimSpace(ctx.FormValue("state"))
	d := s.basePage(ctx)
	d.Title = project + " · Ship"
	d.IsShip = true
	d.Project = project
	d.Ship = s.bot.ListShipBoard(project, state)
	return s.viewPage(ctx, "ship", d)
}

// sessionsScoped lists the workspace's work units.
func (s *Server) sessionsScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.basePage(ctx)
	d.Title = project + " · Sessions"
	d.IsSessions = true
	d.Project = project
	d.Threads = s.projectThreads(project)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	return s.viewPage(ctx, "sessions", d)
}

// worktreesScoped lists the workspace's thread worktrees.
func (s *Server) worktreesScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.basePage(ctx)
	d.Title = project + " · Worktrees"
	d.IsWorktrees = true
	d.Project = project
	d.Worktrees = filterWorktreesProject(s.filterWorktreesVisible(ctx, s.bot.ListWorktrees()), project)
	d.IdleTTLDays = s.cfg.WorktreeIdleTTLDaysValue()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "worktrees", d)
}
