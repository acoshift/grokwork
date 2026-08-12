package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

const (
	actionsRunsTTL         = 15 * time.Second
	actionsWorkflowsTTL    = 60 * time.Second
	actionsCacheMaxEntries = 32
	actionsJobLogMaxRunes  = ghpr.DefaultJobLogRunes
)

// actionsWorkflowRow is one workflow on the Actions page, annotated for dispatch.
type actionsWorkflowRow struct {
	ID           int64
	Name         string
	Path         string
	State        string
	Dispatchable bool
	Inputs       []ghpr.DispatchInput
	// Branches is the select options (locked → only allowed; unlocked → remotes).
	Branches []string
	Locked   bool
}

// actionsRunRow is one recent run with a badge bucket for the table.
type actionsRunRow struct {
	ghpr.RunInfo
	Bucket   string
	ShortSHA string
}

// actionsJobRow is one job on the run detail page.
type actionsJobRow struct {
	ghpr.RunJob
	Bucket    string
	Completed bool
}

type actionsRunsCacheEntry struct {
	runs []ghpr.RunInfo
	at   time.Time
}

type actionsWorkflowsCacheEntry struct {
	workflows []ghpr.WorkflowInfo
	at        time.Time
}

func (s *Server) actionsPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	if _, err := s.projectPath(project); err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		d := s.basePage(ctx)
		d.Title = project + " · Actions"
		d.IsActions = true
		d.Project = project
		d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
		if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
			d.Error = e
		} else {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "actions", d)
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	// Prefer combined picker when present (no-JS friendly, mirrors deploys).
	if full := strings.TrimSpace(ctx.FormValue("repo_full")); full != "" {
		if o, r := pickedRepo(full, owner, repo); o != "" && r != "" {
			owner, repo = o, r
		}
	}
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	d := s.basePage(ctx)
	d.Title = project + " · Actions"
	d.IsActions = true
	d.Project = project
	d.RepoCatalog = catalog
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	if pickErr != nil {
		if d.Error == "" {
			d.Error = pickErr.Error()
		}
		return s.viewPage(ctx, "actions", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo

	d.ActionsWorkflowFilter = strings.TrimSpace(ctx.FormValue("workflow"))
	d.ActionsBranchFilter = strings.TrimSpace(ctx.FormValue("branch"))

	// The shell renders immediately with skeletons; workflows and runs arrive
	// via the partial endpoints (hx-trigger="load") so the slow gh calls never
	// block first paint.
	return s.viewPage(ctx, "actions", d)
}

// actionsWorkflowsPartial renders the workflow register fragment: gh workflow
// list annotated with dispatchability (primary-tree YAML), branch options and
// per-project locks.
func (s *Server) actionsWorkflowsPartial(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	active, pickErr := config.ResolveRepoPicker(catalog, strings.TrimSpace(ctx.FormValue("owner")), strings.TrimSpace(ctx.FormValue("repo")))
	d := s.basePage(ctx)
	d.IsActions = true
	d.Project = project
	if pickErr != nil {
		d.Error = pickErr.Error()
		return s.viewFragment(ctx, "actions", "actions_workflows", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo

	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		d.Error = pathErr.Error()
		return s.viewFragment(ctx, "actions", "actions_workflows", d)
	}
	workflows, wfErr := s.cachedListWorkflows(ctx.Context(), project, active.Owner, active.Repo, repoPath)
	if wfErr != nil {
		d.Error = wfErr.Error()
		return s.viewFragment(ctx, "actions", "actions_workflows", d)
	}
	remoteBranches, _ := ghpr.ListRemoteBranchesWith(ctx.Context(), s.ghRun(), repoPath)
	// One ref resolution for the whole register; per-file reads are one exec each.
	pref := ""
	if s.cfg != nil {
		pref = s.cfg.ProjectPrimaryBranch(project)
	}
	primaryRef, refErr := ghpr.ResolveOriginPrimaryRef(ctx.Context(), s.ghRun(), repoPath, pref)

	rows := make([]actionsWorkflowRow, 0, len(workflows))
	for _, wf := range workflows {
		row := actionsWorkflowRow{
			ID:    wf.ID,
			Name:  wf.Name,
			Path:  wf.Path,
			State: wf.State,
		}
		if refErr == nil {
			if raw, err := ghpr.WorkflowFileAtRefWith(ctx.Context(), s.ghRun(), repoPath, primaryRef, wf.Path); err == nil {
				if spec, err := ghpr.ParseWorkflowDispatch(raw); err == nil && spec.Dispatchable {
					row.Dispatchable = true
					row.Inputs = spec.Inputs
				}
			}
		}
		if branches, locked := s.cfg.ActionsDispatchBranches(project, active.Owner, active.Repo, wf.Path); locked {
			row.Locked = true
			row.Branches = branches
		} else {
			row.Branches = remoteBranches
		}
		rows = append(rows, row)
	}
	d.ActionsWorkflows = rows
	return s.viewFragment(ctx, "actions", "actions_workflows", d)
}

func decorateActionsRuns(runs []ghpr.RunInfo) []actionsRunRow {
	if len(runs) == 0 {
		return nil
	}
	out := make([]actionsRunRow, 0, len(runs))
	for _, r := range runs {
		out = append(out, actionsRunRow{
			RunInfo:  r,
			Bucket:   ghpr.RunBucket(r.Status, r.Conclusion),
			ShortSHA: shortSHA(r.HeadSHA),
		})
	}
	return out
}

func (s *Server) actionsRunsPartial(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	d := s.basePage(ctx)
	d.IsActions = true
	d.Project = project
	d.RepoCatalog = catalog
	if pickErr != nil {
		d.Error = pickErr.Error()
		return s.viewFragment(ctx, "actions", "actions_runs", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	wfFilter := strings.TrimSpace(ctx.FormValue("workflow"))
	branchFilter := strings.TrimSpace(ctx.FormValue("branch"))
	d.ActionsWorkflowFilter = wfFilter
	d.ActionsBranchFilter = branchFilter

	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		d.Error = pathErr.Error()
		return s.viewFragment(ctx, "actions", "actions_runs", d)
	}
	var workflowID int64
	if wfFilter != "" {
		if id, err := strconv.ParseInt(wfFilter, 10, 64); err == nil && id > 0 {
			workflowID = id
		}
	}
	runs, runErr := s.cachedListRuns(ctx.Context(), project, active.Owner, active.Repo, repoPath, ghpr.RunListOpts{
		WorkflowID: workflowID,
		Branch:     branchFilter,
	})
	if runErr != nil {
		d.Error = runErr.Error()
	}
	d.ActionsRuns = decorateActionsRuns(runs)
	return s.viewFragment(ctx, "actions", "actions_runs", d)
}

func (s *Server) actionsRunPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID, err := strconv.ParseInt(strings.TrimSpace(ctx.PathValue("runID")), 10, 64)
	if err != nil || runID <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid run id")
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, catalogErr := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	d := s.basePage(ctx)
	d.Title = project + " · Actions run"
	d.IsActions = true
	d.Project = project
	d.RepoCatalog = catalog
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	if catalogErr != nil {
		if d.Error == "" {
			d.Error = catalogErr.Error()
		}
		return s.viewPage(ctx, "actions_run", d)
	}
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pickErr != nil {
		if d.Error == "" {
			d.Error = pickErr.Error()
		}
		return s.viewPage(ctx, "actions_run", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		if d.Error == "" {
			d.Error = pathErr.Error()
		}
		return s.viewPage(ctx, "actions_run", d)
	}
	detail, detailErr := ghpr.RunDetailWith(ctx.Context(), s.ghRun(), repoPath, runID)
	if detailErr != nil {
		if d.Error == "" {
			d.Error = detailErr.Error()
		}
		return s.viewPage(ctx, "actions_run", d)
	}
	s.fillActionsRunDetail(&d, detail)
	if strings.TrimSpace(ctx.FormValue("fragment")) == "jobs" {
		return s.viewFragment(ctx, "actions_run", "actions_jobs", d)
	}
	return s.viewPage(ctx, "actions_run", d)
}

func (s *Server) fillActionsRunDetail(d *pageData, detail ghpr.RunDetail) {
	d.ActionsRun = detail
	d.ActionsRunBucket = ghpr.RunBucket(detail.Status, detail.Conclusion)
	d.ActionsRunShortSHA = shortSHA(detail.HeadSHA)
	d.ActionsRunLive = !strings.EqualFold(strings.TrimSpace(detail.Status), "completed")
	if len(detail.Jobs) == 0 {
		d.ActionsJobs = nil
		return
	}
	jobs := make([]actionsJobRow, 0, len(detail.Jobs))
	for _, j := range detail.Jobs {
		st := strings.ToLower(strings.TrimSpace(j.Status))
		jobs = append(jobs, actionsJobRow{
			RunJob:    j,
			Bucket:    ghpr.RunBucket(j.Status, j.Conclusion),
			Completed: st == "completed",
		})
	}
	d.ActionsJobs = jobs
}

func (s *Server) actionsJobLog(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID, err := strconv.ParseInt(strings.TrimSpace(ctx.PathValue("runID")), 10, 64)
	if err != nil || runID <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid run id")
	}
	jobID, err := strconv.ParseInt(strings.TrimSpace(ctx.FormValue("job")), 10, 64)
	if err != nil || jobID <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid job id")
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	d := s.basePage(ctx)
	d.IsActions = true
	d.Project = project
	d.ActionsJobID = jobID
	if pickErr != nil {
		d.Error = pickErr.Error()
		return s.viewFragment(ctx, "actions_run", "actions_job_log", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		d.Error = pathErr.Error()
		return s.viewFragment(ctx, "actions_run", "actions_job_log", d)
	}
	// Resolve job URL from run detail when available (for "open full on GitHub").
	if detail, err := ghpr.RunDetailWith(ctx.Context(), s.ghRun(), repoPath, runID); err == nil {
		for _, j := range detail.Jobs {
			if j.ID == jobID {
				d.ActionsJobURL = j.URL
				break
			}
		}
	}
	logText, logErr := ghpr.JobLogWith(ctx.Context(), s.ghRun(), repoPath, runID, jobID, actionsJobLogMaxRunes)
	if logErr != nil {
		d.Error = logErr.Error()
		return s.viewFragment(ctx, "actions_run", "actions_job_log", d)
	}
	// JobLogWith already tail-caps; flag when original might have been longer by
	// re-fetching is expensive — show the open-on-GitHub link when we hit the cap.
	d.ActionsJobLog = logText
	d.ActionsJobLogClipped = len([]rune(logText)) >= actionsJobLogMaxRunes
	d.ActionsJobLogSummary = ghpr.ExtractJobLogSummary(logText)
	return s.viewFragment(ctx, "actions_run", "actions_job_log", d)
}

func (s *Server) postActionsDispatch(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	workflow := strings.TrimSpace(ctx.PostFormValue("workflow"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo,
		"workflow": workflow, "ref": ref,
	}

	project, active, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.actionsRedirect(ctx, project, owner, repo, "", err)
	}
	owner, repo = active.Owner, active.Repo
	detailMap["project"] = project
	detailMap["owner"] = owner
	detailMap["repo"] = repo

	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).GithubWrites {
		denied := errors.New("not allowed to dispatch workflows for this project")
		s.auditAction(ctx, audit.ActionActionsDispatch, denied, detailMap)
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	if workflow == "" {
		bad := errors.New("workflow is required")
		s.auditAction(ctx, audit.ActionActionsDispatch, bad, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", bad)
	}
	if ref == "" {
		bad := errors.New("branch (ref) is required")
		s.auditAction(ctx, audit.ActionActionsDispatch, bad, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", bad)
	}

	workflows, listErr := s.cachedListWorkflows(ctx.Context(), project, owner, repo, cwd)
	if listErr != nil {
		s.auditAction(ctx, audit.ActionActionsDispatch, listErr, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", listErr)
	}
	var found *ghpr.WorkflowInfo
	wfBase := filepath.Base(workflow)
	for i := range workflows {
		w := &workflows[i]
		if w.Path == workflow || filepath.Base(w.Path) == wfBase ||
			strings.EqualFold(filepath.Base(w.Path), wfBase) ||
			strconv.FormatInt(w.ID, 10) == workflow {
			found = w
			break
		}
	}
	if found == nil {
		bad := fmt.Errorf("workflow %q not found", workflow)
		s.auditAction(ctx, audit.ActionActionsDispatch, bad, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", bad)
	}
	detailMap["workflow"] = found.Path

	raw, readErr := ghpr.WorkflowFileAtPrimaryWith(ctx.Context(), s.ghRun(), cwd, found.Path)
	if readErr != nil {
		s.auditAction(ctx, audit.ActionActionsDispatch, readErr, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", readErr)
	}
	spec, parseErr := ghpr.ParseWorkflowDispatch(raw)
	if parseErr != nil || !spec.Dispatchable {
		bad := fmt.Errorf("workflow %q is not dispatchable", filepath.Base(found.Path))
		if parseErr != nil {
			bad = fmt.Errorf("workflow %q is not dispatchable: %w", filepath.Base(found.Path), parseErr)
		}
		s.auditAction(ctx, audit.ActionActionsDispatch, bad, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", bad)
	}

	// Branch lock is the real gate; the form dropdown is only UI.
	if branches, locked := s.cfg.ActionsDispatchBranches(project, owner, repo, found.Path); locked {
		if !slices.Contains(branches, ref) {
			denied := fmt.Errorf("ref %q is not allowed for this workflow (allowed: %s)",
				ref, strings.Join(branches, ", "))
			s.auditAction(ctx, audit.ActionActionsDispatch, denied, detailMap)
			return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
		}
	}

	inputs, inputErr := collectDispatchInputs(ctx, spec)
	if inputErr != nil {
		s.auditAction(ctx, audit.ActionActionsDispatch, inputErr, detailMap)
		return s.actionsRedirect(ctx, project, owner, repo, "", inputErr)
	}
	if len(inputs) > 0 {
		detailMap["inputs"] = len(inputs)
	}

	err = ghpr.DispatchWorkflowWith(ctx.Context(), s.ghRun(), cwd, owner, repo, found.Path, ref, inputs)
	s.auditAction(ctx, audit.ActionActionsDispatch, err, detailMap)
	s.invalidateActionsRunsCache(project, owner, repo)
	if err != nil {
		return s.actionsRedirect(ctx, project, owner, repo, "", err)
	}
	return s.actionsRedirect(ctx, project, owner, repo,
		fmt.Sprintf("Dispatched %s on %s", filepath.Base(found.Path), ref), nil)
}

// collectDispatchInputs reads form fields input.<name> against the workflow
// DispatchSpec. Unknown names are dropped; required inputs without a posted
// value and without a default fail.
func collectDispatchInputs(ctx *hime.Context, spec ghpr.DispatchSpec) (map[string]string, error) {
	if len(spec.Inputs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(spec.Inputs))
	for _, inp := range spec.Inputs {
		key := "input." + inp.Name
		raw, present := formValuePresent(ctx, key)
		typ := strings.ToLower(strings.TrimSpace(inp.Type))
		if typ == "boolean" {
			if present && (raw == "1" || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "on")) {
				out[inp.Name] = "true"
			} else if present {
				out[inp.Name] = "false"
			} else {
				// Unchecked checkbox is absent; treat as false when a default exists or optional.
				if inp.Default != "" {
					out[inp.Name] = inp.Default
				} else {
					out[inp.Name] = "false"
				}
			}
			continue
		}
		val := strings.TrimSpace(raw)
		if !present || val == "" {
			if inp.Default != "" {
				out[inp.Name] = inp.Default
				continue
			}
			if inp.Required {
				return nil, fmt.Errorf("input %q is required", inp.Name)
			}
			continue
		}
		out[inp.Name] = val
	}
	return out, nil
}

func formValuePresent(ctx *hime.Context, key string) (string, bool) {
	if ctx == nil || ctx.Request == nil {
		return "", false
	}
	if err := ctx.Request.ParseForm(); err != nil {
		return "", false
	}
	if vs, ok := ctx.Request.PostForm[key]; ok {
		if len(vs) == 0 {
			return "", true
		}
		return vs[0], true
	}
	return "", false
}

func (s *Server) actionsRedirect(ctx *hime.Context, project, owner, repo, okMsg string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	u := fmt.Sprintf("/projects/%s/actions", url.PathEscape(project))
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func (s *Server) cachedListWorkflows(ctx context.Context, project, owner, repo, path string) ([]ghpr.WorkflowInfo, error) {
	key := project + "\x00" + owner + "\x00" + repo
	now := time.Now()
	s.actionsWorkflowsMu.Lock()
	if e, ok := s.actionsWorkflows[key]; ok && now.Sub(e.at) < actionsWorkflowsTTL {
		out := append([]ghpr.WorkflowInfo(nil), e.workflows...)
		s.actionsWorkflowsMu.Unlock()
		return out, nil
	}
	s.actionsWorkflowsMu.Unlock()

	workflows, err := ghpr.ListWorkflowsWith(ctx, s.ghRun(), path)
	if err != nil {
		return nil, err
	}

	s.actionsWorkflowsMu.Lock()
	if s.actionsWorkflows == nil {
		s.actionsWorkflows = map[string]actionsWorkflowsCacheEntry{}
	}
	for k, e := range s.actionsWorkflows {
		if now.Sub(e.at) >= actionsWorkflowsTTL {
			delete(s.actionsWorkflows, k)
		}
	}
	if len(s.actionsWorkflows) >= actionsCacheMaxEntries {
		oldest, oldestAt := "", now
		for k, e := range s.actionsWorkflows {
			if e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.actionsWorkflows, oldest)
	}
	s.actionsWorkflows[key] = actionsWorkflowsCacheEntry{
		workflows: append([]ghpr.WorkflowInfo(nil), workflows...),
		at:        now,
	}
	s.actionsWorkflowsMu.Unlock()
	return workflows, nil
}

func (s *Server) cachedListRuns(ctx context.Context, project, owner, repo, path string, opts ghpr.RunListOpts) ([]ghpr.RunInfo, error) {
	key := project + "\x00" + owner + "\x00" + repo + "\x00" +
		strconv.FormatInt(opts.WorkflowID, 10) + "\x00" + strings.TrimSpace(opts.Branch)
	now := time.Now()
	s.actionsRunsMu.Lock()
	if e, ok := s.actionsRuns[key]; ok && now.Sub(e.at) < actionsRunsTTL {
		out := append([]ghpr.RunInfo(nil), e.runs...)
		s.actionsRunsMu.Unlock()
		return out, nil
	}
	s.actionsRunsMu.Unlock()

	runs, err := ghpr.ListRunsWith(ctx, s.ghRun(), path, opts)
	if err != nil {
		return nil, err
	}

	s.actionsRunsMu.Lock()
	if s.actionsRuns == nil {
		s.actionsRuns = map[string]actionsRunsCacheEntry{}
	}
	for k, e := range s.actionsRuns {
		if now.Sub(e.at) >= actionsRunsTTL {
			delete(s.actionsRuns, k)
		}
	}
	if len(s.actionsRuns) >= actionsCacheMaxEntries {
		oldest, oldestAt := "", now
		for k, e := range s.actionsRuns {
			if e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.actionsRuns, oldest)
	}
	s.actionsRuns[key] = actionsRunsCacheEntry{
		runs: append([]ghpr.RunInfo(nil), runs...),
		at:   now,
	}
	s.actionsRunsMu.Unlock()
	return runs, nil
}

func (s *Server) invalidateActionsRunsCache(project, owner, repo string) {
	project = strings.TrimSpace(project)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if project == "" || owner == "" || repo == "" {
		return
	}
	prefix := project + "\x00" + owner + "\x00" + repo + "\x00"
	s.actionsRunsMu.Lock()
	defer s.actionsRunsMu.Unlock()
	for k := range s.actionsRuns {
		if strings.HasPrefix(k, prefix) {
			delete(s.actionsRuns, k)
		}
	}
}

// runBucketBadge maps ghpr.RunBucket values onto layout badge CSS classes.
// pending → status-running (same blue as .live); pass → status-done (green).
func runBucketBadge(bucket string) string {
	switch bucket {
	case "pass":
		return "status-done"
	case "fail":
		return "status-error"
	case "pending":
		return "status-running"
	case "skipping":
		return "status-cancelled"
	case "cancel":
		return "status-cancelled"
	default:
		return ""
	}
}
