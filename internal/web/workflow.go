package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/sessionstore"
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
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
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
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if listErr != nil {
		d.Error = listErr.Error()
	} else if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
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
	var index ghpr.DiffIndex
	raw, diffErr := s.prPatch(ctx.Context(), cwd, selector)
	if diffErr == nil {
		index = ghpr.StatPatch(raw, ghpr.DefaultMaxIndexFiles)
	}
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("Diff · %s/%s#%d", owner, repo, n)
	d.IsShip = true
	d.Project = project
	d.ActiveOwner = owner
	d.ActiveRepo = repo
	d.PRNumber = n
	extra := url.Values{}
	if project != "" {
		extra.Set("project", project)
	}
	fragBase := fmt.Sprintf("/prs/%s/%s/%d/diff/file", url.PathEscape(owner), url.PathEscape(repo), n)
	d.DiffReview = buildDiffReview(index, fmt.Sprintf("pr:%s/%s#%d", owner, repo, n), func(f ghpr.FileStat) string {
		return fragBase + "?" + fragQuery(f, extra)
	})
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
	cwd, project := s.resolveSessionDiffCwd(ent, threadID)
	base := s.sessionDiffBase(ctx.Context(), ent, cwd, ctx.FormValue("base"))
	var index ghpr.DiffIndex
	var diffErr error
	if cwd == "" {
		diffErr = fmt.Errorf("worktree no longer on disk for this session (project=%q)", project)
	} else {
		index, diffErr = ghpr.WorktreeDiffIndexWith(ctx.Context(), s.ghRun(), cwd, base)
	}
	d := s.basePage(ctx)
	d.Title = "Worktree diff · " + threadID
	d.IsSessions = true
	if project == "" {
		project = d.NavProject
	}
	d.Project = project
	d.ThreadID = threadID
	d.DiffBase = base
	extra := url.Values{"base": {base}}
	if project != "" {
		extra.Set("project", project)
	}
	fragBase := "/sessions/" + url.PathEscape(threadID) + "/diff/file"
	d.DiffReview = buildDiffReview(index, "s:"+threadID+":"+base, func(f ghpr.FileStat) string {
		return fragBase + "?" + fragQuery(f, extra)
	})
	if diffErr != nil {
		d.Error = diffErr.Error()
	}
	return s.viewPage(ctx, "diff", d)
}

// sessionDiffBase resolves the diff base ref.
// Priority: ?base= query → tracked PR baseRefName → closest local base
// (fewest commits from merge-base to HEAD) → HEAD.
// Hardcoding origin/main is wrong for backports (e.g. → prod): the worktree
// then looks like it adds/removes hundreds of unrelated files.
func (s *Server) sessionDiffBase(ctx context.Context, ent sessionstore.Entry, cwd, requested string) string {
	if b := strings.TrimSpace(requested); b != "" {
		if cwd != "" {
			return ghpr.PreferOriginRef(ctx, s.ghRun(), cwd, b)
		}
		return b
	}
	preferred := s.sessionPRBaseName(ctx, ent, cwd)
	if cwd != "" {
		return ghpr.ResolveDiffBaseRef(ctx, s.ghRun(), cwd, preferred)
	}
	if preferred != "" {
		// No repo to probe origin/*; keep the PR's short name.
		return preferred
	}
	return "HEAD"
}

// sessionPRBaseName returns the GitHub PR base branch short name when known
// (e.g. "prod"), without origin/ prefix.
func (s *Server) sessionPRBaseName(ctx context.Context, ent sessionstore.Entry, cwd string) string {
	ent.NormalizePRs()
	pr, ok := ent.PrimaryPR()
	if !ok {
		return ""
	}
	sel := pr.Selector()
	if sel == "" {
		return ""
	}
	// Prefer worktree cwd so gh uses the right repo; fall back to main checkout.
	repoDir := strings.TrimSpace(cwd)
	if repoDir == "" {
		repoDir = strings.TrimSpace(ent.MainCwd)
	}
	if repoDir == "" && ent.Project != "" {
		repoDir, _ = s.cfg.ProjectPath(ent.Project)
	}
	if repoDir == "" {
		return ""
	}
	base, err := ghpr.PRBaseRefWith(ctx, s.ghRun(), repoDir, sel)
	if err != nil {
		return ""
	}
	return base
}

// resolveSessionDiffCwd picks the session's on-disk git worktree for the
// worktree-diff page. It never falls back to the main project checkout or the
// bot process cwd — a removed/pruned worktree returns empty so the UI can show
// an error instead of an empty or misleading main-branch diff.
func (s *Server) resolveSessionDiffCwd(ent sessionstore.Entry, threadID string) (cwd, project string) {
	project = strings.TrimSpace(ent.Project)
	mainCwd := strings.TrimSpace(ent.MainCwd)
	if mainCwd == "" && project != "" {
		if p, ok := s.cfg.ProjectPath(project); ok {
			mainCwd = p
		}
	}

	// Canonical / healed worktree under dataDir (requires real git root).
	if path, onDisk := gitworktree.ResolveSessionWorktreePath(s.cfg.DataDir, project, threadID, ent.Cwd, mainCwd); onDisk {
		return path, project
	}

	// Session metadata may have lost project/cwd while the worktree still exists.
	if d, ok := gitworktree.FindOnDiskByUnitID(s.cfg.DataDir, threadID); ok && gitworktree.IsRepo(d.Path) {
		if project == "" {
			project = d.Project
		}
		return d.Path, project
	}

	// Live session cwd (includes worktreeIsolation=false where cwd is the main
	// checkout). Do not invent mainCwd when ent.Cwd is gone — that path is the
	// "worktree already removed" case and must surface as an error.
	if c := strings.TrimSpace(ent.Cwd); gitworktree.IsRepo(c) {
		return c, project
	}
	return "", project
}

