package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/linear"
)

// ghRun returns the injectable Runner (tests set s.ghRunner).
func (s *Server) ghRun() ghpr.Runner {
	if s != nil && s.ghRunner != nil {
		return s.ghRunner
	}
	return nil // ghpr *With treats nil as default
}

func (s *Server) linearClient(project string) *linear.Client {
	key := s.cfg.ProjectLinearAPIKey(project)
	if s.linearNew != nil {
		return s.linearNew(key)
	}
	return linear.New(key)
}

func (s *Server) projectPath(name string) (string, error) {
	path, ok := s.cfg.ProjectPath(name)
	if !ok {
		return "", fmt.Errorf("unknown project %q", name)
	}
	return path, nil
}

// resolveCatalogRepo ensures owner/repo is in the project's GitHub catalog (authorization boundary).
// If project is empty, finds the first project whose catalog contains the repo.
func (s *Server) resolveCatalogRepo(ctx context.Context, project, owner, repo string) (proj string, ref config.GitHubRepoRef, cwd string, err error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	project = strings.TrimSpace(project)
	if owner == "" || repo == "" {
		return "", config.GitHubRepoRef{}, "", fmt.Errorf("owner and repo are required")
	}
	if project != "" {
		cwd, err = s.projectPath(project)
		if err != nil {
			return "", config.GitHubRepoRef{}, "", err
		}
		cat, cErr := s.cfg.ProjectRepoCatalogWith(ctx, project, nil)
		if cErr != nil {
			return "", config.GitHubRepoRef{}, "", cErr
		}
		ref, err = config.ResolveRepoPicker(cat, owner, repo)
		if err != nil {
			return "", config.GitHubRepoRef{}, "", fmt.Errorf("repository %s/%s is not in project %q catalog", owner, repo, project)
		}
		return project, ref, cwd, nil
	}
	for _, name := range s.cfg.ProjectNames() {
		cat, cErr := s.cfg.ProjectRepoCatalogWith(ctx, name, nil)
		if cErr != nil || len(cat) == 0 {
			continue
		}
		r, rErr := config.ResolveRepoPicker(cat, owner, repo)
		if rErr != nil {
			continue
		}
		p, pErr := s.projectPath(name)
		if pErr != nil {
			continue
		}
		return name, r, p, nil
	}
	return "", config.GitHubRepoRef{}, "", fmt.Errorf("repository %s/%s is not in any project catalog", owner, repo)
}

func (s *Server) issuesList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	path, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	// Allow test injection of catalog via ghRunner only — catalog still from config.
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, err := config.ResolveRepoPicker(catalog, owner, repo)
	if err != nil {
		// still render page with error if catalog empty
		d := s.basePage(ctx)
		d.Title = "Issues · " + project
		d.IsIssues = true
		d.Project = project
		d.RepoCatalog = catalog
		d.Error = err.Error()
		return s.viewPage(ctx, "issues", d)
	}
	state := strings.TrimSpace(ctx.FormValue("state"))
	if state == "" {
		state = "open"
	}
	issues, listErr := ghpr.ListIssuesWith(ctx.Context(), s.ghRun(), path, ghpr.IssueListOpts{
		Owner: active.Owner,
		Repo:  active.Repo,
		State: state,
		Limit: 40,
	})
	d := s.basePage(ctx)
	d.Title = "Issues · " + project
	d.IsIssues = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.IssueState = state
	d.Issues = issues
	d.LinearEnabled = s.cfg.ProjectLinearEnabled(project)
	if listErr != nil {
		d.Error = listErr.Error()
	}
	return s.viewPage(ctx, "issues", d)
}

func (s *Server) issueDetail(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	nStr := strings.TrimSpace(ctx.PathValue("n"))
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	path, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	_, active, _, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		// default first catalog entry when owner/repo omitted
		if owner == "" && repo == "" && len(catalog) > 0 {
			active = catalog[0]
		} else {
			return ctx.Status(http.StatusForbidden).Error(err.Error())
		}
	}
	info, viewErr := ghpr.ViewIssueWith(ctx.Context(), s.ghRun(), path, n, active.Owner, active.Repo)
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("%s/%s#%d", active.Owner, active.Repo, n)
	d.IsIssues = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.Issue = info
	d.LinearEnabled = s.cfg.ProjectLinearEnabled(project)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if viewErr != nil {
		d.Error = viewErr.Error()
	}
	d.ShowFixPicker = ctx.FormValue("picker") == "1"
	s.attachFixPicker(&d, project, active.Owner, active.Repo, n, "")
	if d.ShowFixPicker || len(d.FixHits) > 1 {
		d.ShowFixPicker = true
	}
	return s.viewPage(ctx, "issue_detail", d)
}

func (s *Server) linearList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if !s.cfg.ProjectLinearEnabled(project) {
		return ctx.Status(http.StatusNotFound).Error("Linear is not enabled for this project")
	}
	if !s.cfg.ProjectLinearCanResolve(project) {
		d := s.basePage(ctx)
		d.Title = "Linear · " + project
		d.IsLinear = true
		d.Project = project
		d.Error = "Linear enabled but no API key (config or LINEAR_API_KEY_<PROJECT>)"
		return s.viewPage(ctx, "linear_issues", d)
	}
	team := s.cfg.ProjectLinearTeamKey(project)
	client := s.linearClient(project)
	issues, listErr := client.ListTeamIssues(ctx.Context(), team, 40)
	d := s.basePage(ctx)
	d.Title = "Linear · " + project
	d.IsLinear = true
	d.Project = project
	d.LinearTeam = team
	d.LinearIssues = issues
	d.LinearEnabled = true
	if listErr != nil {
		d.Error = listErr.Error()
	}
	return s.viewPage(ctx, "linear_issues", d)
}

func (s *Server) linearDetail(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("identifier"))
	if !s.cfg.ProjectLinearEnabled(project) {
		return ctx.Status(http.StatusNotFound).Error("Linear is not enabled for this project")
	}
	client := s.linearClient(project)
	issue, err := client.GetByIdentifier(ctx.Context(), id)
	if strings.TrimSpace(issue.Identifier) == "" {
		issue.Identifier = id
	}
	d := s.basePage(ctx)
	d.Title = id + " · " + project
	d.IsLinear = true
	d.Project = project
	d.LinearTeam = s.cfg.ProjectLinearTeamKey(project)
	d.LinearIssue = issue
	d.LinearEnabled = true
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if err != nil {
		d.Error = err.Error()
	}
	d.ShowFixPicker = ctx.FormValue("picker") == "1"
	s.attachFixPicker(&d, project, "", "", 0, id)
	if d.ShowFixPicker || len(d.FixHits) > 1 {
		d.ShowFixPicker = true
	}
	return s.viewPage(ctx, "linear_detail", d)
}

func (s *Server) prDetail(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	nStr := strings.TrimSpace(ctx.PathValue("n"))
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 || owner == "" || repo == "" {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR path")
	}
	project := strings.TrimSpace(ctx.FormValue("project"))
	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner, repo = ref.Owner, ref.Repo
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	detail, viewErr := ghpr.ViewPRDetailWith(ctx.Context(), s.ghRun(), cwd, selector)
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("%s/%s#%d", owner, repo, n)
	d.IsShip = true
	d.Project = project
	d.ActiveOwner = owner
	d.ActiveRepo = repo
	d.PR = detail
	d.PRNumber = n
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if viewErr != nil {
		d.Error = viewErr.Error()
	}
	d.ShowFixPicker = ctx.FormValue("picker") == "1"
	if s.bot != nil && project != "" {
		d.FixHits = s.bot.FindByPR(project, owner, repo, n, false)
		if d.ShowFixPicker || len(d.FixHits) > 1 {
			d.ShowFixPicker = true
		}
	}
	return s.viewPage(ctx, "pr_detail", d)
}

func (s *Server) prDiffPage(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	nStr := strings.TrimSpace(ctx.PathValue("n"))
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.FormValue("project"))
	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner, repo = ref.Owner, ref.Repo
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	diff, diffErr := ghpr.PRDiffWith(ctx.Context(), s.ghRun(), cwd, selector, ghpr.DiffOpts{})
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("Diff · %s/%s#%d", owner, repo, n)
	d.IsShip = true
	d.Project = project
	d.ActiveOwner = owner
	d.ActiveRepo = repo
	d.PRNumber = n
	d.Diff = diff
	if diffErr != nil {
		d.Error = diffErr.Error()
	}
	return s.viewPage(ctx, "diff", d)
}

func (s *Server) sessionDiffPage(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	ent, ok := s.sessions.Get(threadID)
	if !ok {
		return ctx.Status(http.StatusNotFound).Error("unknown session/thread")
	}
	cwd := strings.TrimSpace(ent.Cwd)
	// Heal stale absolute worktree paths after dataDir rename (grok-discord → grokwork).
	if path, onDisk := gitworktree.ResolveSessionWorktreePath(s.cfg.DataDir, ent.Project, threadID, ent.Cwd, ent.MainCwd); onDisk {
		cwd = path
	} else if cwd == "" {
		if p, ok := s.cfg.ProjectPath(ent.Project); ok {
			cwd = p
		}
	}
	base := strings.TrimSpace(ctx.FormValue("base"))
	if base == "" {
		base = "HEAD"
		// prefer main branch merge-base style: origin/main if set
		if ent.MainCwd != "" {
			base = "origin/main"
		}
	}
	diff, diffErr := ghpr.WorktreeDiffWith(ctx.Context(), s.ghRun(), cwd, base, ghpr.DiffCaps{})
	d := s.basePage(ctx)
	d.Title = "Worktree diff · " + threadID
	d.IsSessions = true
	d.Project = ent.Project
	d.ThreadID = threadID
	d.Diff = diff
	d.DiffBase = base
	if diffErr != nil {
		d.Error = diffErr.Error()
	}
	return s.viewPage(ctx, "diff", d)
}

func (s *Server) issuesIndex(ctx *hime.Context) error {
	// Landing: pick a project
	d := s.basePage(ctx)
	d.Title = "Issues"
	d.IsIssues = true
	d.Config = s.cfg.Snapshot()
	return s.viewPage(ctx, "issues_index", d)
}
