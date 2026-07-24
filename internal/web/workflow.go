package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/reviewstore"
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
// cwd is the local git checkout for that repo (project root for single-repo projects;
// project root/<repo> for multi-repo folder layouts).
func (s *Server) resolveCatalogRepo(ctx context.Context, project, owner, repo string) (proj string, ref config.GitHubRepoRef, cwd string, err error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	project = strings.TrimSpace(project)
	if owner == "" || repo == "" {
		return "", config.GitHubRepoRef{}, "", fmt.Errorf("owner and repo are required")
	}
	if project != "" {
		root, pErr := s.projectPath(project)
		if pErr != nil {
			return "", config.GitHubRepoRef{}, "", pErr
		}
		cat, cErr := s.cfg.ProjectRepoCatalogWith(ctx, project, nil)
		if cErr != nil {
			return "", config.GitHubRepoRef{}, "", cErr
		}
		ref, err = config.ResolveRepoPicker(cat, owner, repo)
		if err != nil {
			return "", config.GitHubRepoRef{}, "", fmt.Errorf("repository %s/%s is not in project %q catalog", owner, repo, project)
		}
		// Prefer the local checkout for the selected repo (multi-repo folder
		// layout). Fall back to the project root so gh --repo callers still work
		// when a child checkout is missing or the path is not a git root.
		if local, lErr := gitworktree.ResolveLocalRepo(ctx, root, ref.Owner, ref.Repo); lErr == nil {
			return project, ref, local, nil
		}
		return project, ref, root, nil
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
		root, pErr := s.projectPath(name)
		if pErr != nil {
			continue
		}
		local, lErr := gitworktree.ResolveLocalRepo(ctx, root, r.Owner, r.Repo)
		if lErr != nil {
			// Catalog match is enough for authorization; fall back to project root
			// so gh --repo callers still work when the local checkout is missing.
			return name, r, root, nil
		}
		return name, r, local, nil
	}
	return "", config.GitHubRepoRef{}, "", fmt.Errorf("repository %s/%s is not in any project catalog", owner, repo)
}

// issuesList renders the issues page shell immediately. The table is loaded via
// /partials/issues/table (hx-trigger=load) so navigation is not blocked on gh.
func (s *Server) issuesList(ctx *hime.Context) error {
	d, err := s.issuesPageShell(ctx)
	if err != nil {
		return err
	}
	return s.viewPage(ctx, "issues", d)
}

// partialIssuesTable streams the issues table (and Fix bar) after the shell paints.
func (s *Server) partialIssuesTable(ctx *hime.Context) error {
	d, err := s.issuesPageShell(ctx)
	if err != nil {
		return err
	}
	if d.Error != "" && d.ActiveOwner == "" {
		// Catalog/access error already set; still render empty table region.
		return s.viewFragment(ctx, "issues", "issues_table", d)
	}
	s.loadIssuesInto(&d, ctx)
	return s.viewFragment(ctx, "issues", "issues_table", d)
}

// issuesPageShell resolves project access, catalog, and filter UI state without
// calling GitHub. Shared by the full page and the table partial.
func (s *Server) issuesPageShell(ctx *hime.Context) (pageData, error) {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if project == "" {
		// Partial uses query param so it can be shared with filter form values.
		project = strings.TrimSpace(ctx.FormValue("project"))
	}
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return pageData{}, ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	if _, err := s.projectPath(project); err != nil {
		return pageData{}, ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		return pageData{}, ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, err := config.ResolveRepoPicker(catalog, owner, repo)
	d := s.basePage(ctx)
	d.Title = project + " · Issues"
	d.IsIssues = true
	d.Project = project
	d.RepoCatalog = catalog
	d.LinearEnabled = s.cfg.ProjectLinearEnabled(project)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if err != nil {
		// Still render page with error if catalog empty / bad picker.
		d.Error = err.Error()
		return d, nil
	}
	state := strings.TrimSpace(ctx.FormValue("state"))
	if state == "" {
		state = "open"
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.IssueState = state
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	return d, nil
}

const (
	issueListTTL        = 20 * time.Second
	issueListMaxEntries = 32
	issueListLimit      = 40
)

type issueListCacheEntry struct {
	issues []ghpr.IssueInfo
	at     time.Time
}

// loadIssuesInto fetches (or reuses a short-TTL cache of) GitHub issues and
// applies the FIXING overlay / state=fixing filter.
func (s *Server) loadIssuesInto(d *pageData, ctx *hime.Context) {
	if d == nil || d.ActiveOwner == "" || d.ActiveRepo == "" {
		return
	}
	project := d.Project
	path, err := s.projectPath(project)
	if err != nil {
		d.Error = err.Error()
		return
	}
	state := d.IssueState
	if state == "" {
		state = "open"
	}
	// "fixing" is a grokwork overlay: load open GitHub issues, keep those with active Fixes sessions.
	ghState := state
	if state == "fixing" {
		ghState = "open"
	}
	cacheKey := project + "\x00" + d.ActiveOwner + "\x00" + d.ActiveRepo + "\x00" + ghState

	issues, listErr := s.cachedListIssues(ctx.Context(), cacheKey, path, ghpr.IssueListOpts{
		Owner: d.ActiveOwner,
		Repo:  d.ActiveRepo,
		State: ghState,
		Limit: issueListLimit,
	})
	if listErr == nil {
		issues = s.annotateGitHubIssueWorkState(project, d.ActiveOwner, d.ActiveRepo, issues)
		if state == "fixing" {
			filtered := issues[:0]
			for _, iss := range issues {
				if iss.WorkState == bot.IssueWorkStateFixing {
					filtered = append(filtered, iss)
				}
			}
			issues = filtered
		}
		d.Issues = issues
	} else if d.Error == "" {
		d.Error = listErr.Error()
	}
}

func (s *Server) cachedListIssues(ctx context.Context, key, path string, opts ghpr.IssueListOpts) ([]ghpr.IssueInfo, error) {
	now := time.Now()
	s.issueListMu.Lock()
	if e, ok := s.issueLists[key]; ok && now.Sub(e.at) < issueListTTL {
		issues := append([]ghpr.IssueInfo(nil), e.issues...)
		s.issueListMu.Unlock()
		return issues, nil
	}
	s.issueListMu.Unlock()

	issues, err := ghpr.ListIssuesWith(ctx, s.ghRun(), path, opts)
	if err != nil {
		// Do not cache failures — retries should hit GitHub again.
		return nil, err
	}

	s.issueListMu.Lock()
	if s.issueLists == nil {
		s.issueLists = map[string]issueListCacheEntry{}
	}
	for k, e := range s.issueLists {
		if now.Sub(e.at) >= issueListTTL {
			delete(s.issueLists, k)
		}
	}
	if len(s.issueLists) >= issueListMaxEntries {
		oldest, oldestAt := "", now
		for k, e := range s.issueLists {
			if e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.issueLists, oldest)
	}
	s.issueLists[key] = issueListCacheEntry{
		issues: append([]ghpr.IssueInfo(nil), issues...),
		at:     now,
	}
	s.issueListMu.Unlock()
	return issues, nil
}

// invalidateIssueListCache drops cached lists for a repo (after write mutations).
func (s *Server) invalidateIssueListCache(project, owner, repo string) {
	project = strings.TrimSpace(project)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if project == "" || owner == "" || repo == "" {
		return
	}
	prefix := project + "\x00" + owner + "\x00" + repo + "\x00"
	s.issueListMu.Lock()
	defer s.issueListMu.Unlock()
	for k := range s.issueLists {
		if strings.HasPrefix(k, prefix) {
			delete(s.issueLists, k)
		}
	}
}

func (s *Server) issueDetail(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
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
	if viewErr == nil {
		annotated := s.annotateGitHubIssueWorkState(project, active.Owner, active.Repo, []ghpr.IssueInfo{info})
		if len(annotated) > 0 {
			info = annotated[0]
		}
	}
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

// annotateGitHubIssueWorkState marks issues FIXING when a non-terminal session binds them with Fixes.
func (s *Server) annotateGitHubIssueWorkState(project, owner, repo string, issues []ghpr.IssueInfo) []ghpr.IssueInfo {
	if s == nil || s.bot == nil || len(issues) == 0 {
		return issues
	}
	active := s.bot.ActiveFixGitHubIssues(project, owner, repo)
	if len(active) == 0 {
		return issues
	}
	for i := range issues {
		// Only overlay FIXING on still-open GitHub issues.
		if !strings.EqualFold(issues[i].State, "open") {
			continue
		}
		if _, ok := active[issues[i].Number]; ok {
			issues[i].WorkState = bot.IssueWorkStateFixing
		}
	}
	return issues
}

// annotateLinearIssueWorkState marks Linear issues FIXING for active Fixes sessions.
func (s *Server) annotateLinearIssueWorkState(project string, issues []linear.Issue) []linear.Issue {
	if s == nil || s.bot == nil || len(issues) == 0 {
		return issues
	}
	active := s.bot.ActiveFixLinearIssues(project)
	if len(active) == 0 {
		return issues
	}
	for i := range issues {
		id := sessionstore.NormalizeLinearIdentifier(issues[i].Identifier)
		if _, ok := active[id]; ok {
			issues[i].WorkState = bot.IssueWorkStateFixing
		}
	}
	return issues
}

func (s *Server) linearList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
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
	if listErr == nil {
		issues = s.annotateLinearIssueWorkState(project, issues)
	}
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
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	id := strings.TrimSpace(ctx.PathValue("identifier"))
	if !s.cfg.ProjectLinearEnabled(project) {
		return ctx.Status(http.StatusNotFound).Error("Linear is not enabled for this project")
	}
	client := s.linearClient(project)
	issue, err := client.GetByIdentifier(ctx.Context(), id)
	if strings.TrimSpace(issue.Identifier) == "" {
		issue.Identifier = id
	}
	if err == nil {
		annotated := s.annotateLinearIssueWorkState(project, []linear.Issue{issue})
		if len(annotated) > 0 {
			issue = annotated[0]
		}
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
	d, err := s.prDetailPageData(ctx, true)
	if err != nil {
		return err
	}
	return s.viewPage(ctx, "pr_detail", d)
}

// partialPRGates re-renders the shippability strip (Checks / reviews / mergeable)
// after an sse:ship tick. The PR poller updates session store checks; this partial
// re-views GitHub so the detail page reflects live CI without a full reload.
func (s *Server) partialPRGates(ctx *hime.Context) error {
	d, err := s.prDetailPageData(ctx, false)
	if err != nil {
		return err
	}
	return s.viewFragment(ctx, "pr_detail", "pr_gates", d)
}

// prDetailPageData loads PR detail for the full page or the gates partial.
// full=true attaches flash/error query params, fix picker, team review table,
// and reviewer options; the gates partial only needs the strip fields.
func (s *Server) prDetailPageData(ctx *hime.Context, full bool) (pageData, error) {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	nStr := strings.TrimSpace(ctx.PathValue("n"))
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 || owner == "" || repo == "" {
		return pageData{}, ctx.Status(http.StatusBadRequest).Error("invalid PR path")
	}
	project := strings.TrimSpace(ctx.FormValue("project"))
	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return pageData{}, ctx.Status(http.StatusForbidden).Error(err.Error())
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
	if full {
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
	} else if viewErr != nil {
		d.Error = viewErr.Error()
	}
	if store := s.reviewsStore(); store != nil {
		bucket := store.ListForPR(owner, repo, n)
		head := detail.HeadSHA
		if head == "" {
			head = bucket.LastHeadSHA
		}
		label, _, _ := reviewstore.TeamRollup(bucket, head)
		d.TeamRollup = label
		d.TeamRollupText = teamRollupText(label)
		d.TeamRollupBadge = teamRollupBadge(label)
		if full {
			d.TeamReviews = buildTeamReviewRows(bucket, head)
			for _, req := range bucket.Requests {
				if req.Status == reviewstore.StatusPending {
					d.TeamPendingRequests = append(d.TeamPendingRequests, req)
				}
			}
		}
	}
	if full && d.CanPRReview {
		d.ReviewerOptions = s.reviewerOptions(project)
	}
	if viewErr == nil {
		d.PRGates, d.PRShipReady = buildPRGates(detail, d.TeamRollup)
	}
	return d, nil
}

// prGate is one fact on the PR detail shippability strip.
type prGate struct {
	Label string
	Value string
	Class string // "ok" | "warn" | "err" | "" (neutral / pending)
	Hint  string
}

// buildPRGates turns the PR snapshot + team rollup into the header gate strip
// and reports whether every gate is green (drives the merge affordance).
func buildPRGates(pr ghpr.PRDetail, teamRollup string) ([]prGate, bool) {
	checks := strings.TrimSpace(pr.Checks)
	cg := prGate{Label: "Checks", Value: checks}
	switch {
	case checks == "":
		cg.Value = "—"
		cg.Hint = "no CI reported"
	case ghpr.ChecksFailing(checks):
		cg.Class = "err"
		cg.Hint = "failing"
	case strings.Contains(checks, "…"):
		cg.Hint = "running"
	default:
		cg.Class = "ok"
		cg.Hint = "green"
	}

	tg := prGate{Label: "Team review", Value: teamRollupText(teamRollup)}
	switch teamRollup {
	case reviewstore.RollupApproved:
		tg.Class = "ok"
	case reviewstore.RollupChangesRequested:
		tg.Class = "warn"
	case reviewstore.RollupStaleApprovals:
		tg.Hint = "head moved since approval"
	case reviewstore.RollupReviewRequested:
		tg.Hint = "waiting on reviewer"
	}

	gg := prGate{Label: "GitHub review", Value: pr.ReviewDecision}
	switch pr.ReviewDecision {
	case "APPROVED":
		gg.Class = "ok"
	case "CHANGES_REQUESTED":
		gg.Class = "warn"
	case "":
		gg.Value = "—"
		gg.Hint = "not required"
	}

	mg := prGate{Label: "Mergeable", Value: pr.Mergeable}
	switch pr.Mergeable {
	case "MERGEABLE":
		mg.Class = "ok"
	case "CONFLICTING":
		mg.Class = "err"
		mg.Hint = "resolve conflicts"
	case "":
		mg.Value = "—"
	}

	checksOK := cg.Class == "ok" || checks == ""
	approved := teamRollup == reviewstore.RollupApproved || pr.ReviewDecision == "APPROVED"
	ready := pr.State == "OPEN" && !pr.IsDraft && checksOK &&
		pr.Mergeable == "MERGEABLE" && approved
	return []prGate{cg, tg, gg, mg}, ready
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
	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
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
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
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

	// Canonical / healed worktree under worktrees root (requires real git root).
	if path, onDisk := gitworktree.ResolveSessionWorktreePath(s.cfg.WorktreesRoot(), project, threadID, ent.Cwd, mainCwd); onDisk {
		return path, project
	}

	// Session metadata may have lost project/cwd while the worktree still exists.
	if d, ok := gitworktree.FindOnDiskByUnitID(s.cfg.WorktreesRoot(), threadID); ok && gitworktree.IsRepo(d.Path) {
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
