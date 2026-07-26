package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// deployCell is one (service, environment) square of the deploy board.
type deployCell struct {
	Env        string
	Deployable bool
	Steps      []deploy.ResolvedStep
	// Reason explains a non-deployable cell (narrowed away, no steps).
	Reason string
}

// deployRow is one service's row across every environment the manifest declares.
type deployRow struct {
	Service string
	Dir     string
	Cells   []deployCell
}

// deploysPage renders the pipeline a project declares in its repo.
//
// Read-only: it shows what would run, not a trigger. The manifest is read from
// the primary branch tip rather than the shared checkout's working tree, because
// the working tree is routinely dirty with other agents' edits and would show a
// pipeline that is not what a deploy of the primary branch will actually run.
func (s *Server) deploysPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}

	d := s.basePage(ctx)
	d.Title = project + " · Deploys"
	d.IsDeploys = true
	d.Project = project
	d.DeployManifestPath = deploy.DefaultManifestPath
	if p := strings.TrimSpace(s.cfg.ProjectDeployManifestPath(project)); p != "" {
		d.DeployManifestPath = p
	}
	// Filled before every early return: the generate form is most useful exactly
	// when the manifest is missing or broken.
	d.CanGenerateManifest = d.CanStartSession &&
		s.cfg.ResolveCapabilities(project, d.UserID, nil).CanShip()
	s.attachModelPicker(&d, project, s.cfg.TaskModel())
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}

	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		// A project with no catalog and no discoverable remote is still
		// deployable from its own checkout; only a hard config error lands here.
		if d.Error == "" {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "deploys", d)
	}
	d.RepoCatalog = catalog

	repoPath := root
	if len(catalog) > 0 {
		// Mirror the commits browser: a repo-less project must still work, so the
		// picker is only consulted when there is something to pick from.
		owner, repo := pickedRepo(ctx.FormValue("repo_full"), ctx.FormValue("owner"), ctx.FormValue("repo"))
		active, err := config.ResolveRepoPicker(catalog, owner, repo)
		if err != nil {
			if d.Error == "" {
				d.Error = err.Error()
			}
			return s.viewPage(ctx, "deploys", d)
		}
		d.ActiveOwner = active.Owner
		d.ActiveRepo = active.Repo
		resolved, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
		if pathErr != nil {
			if d.Error == "" {
				d.Error = pathErr.Error()
			}
			return s.viewPage(ctx, "deploys", d)
		}
		repoPath = resolved
	} else if !gitworktree.IsRepo(repoPath) {
		if d.Error == "" {
			d.Error = "project path is not a git repository"
		}
		return s.viewPage(ctx, "deploys", d)
	}

	ref := gitworktree.PrimaryStartRef(ctx.Context(), repoPath)
	d.DeployRef = ref
	if sha, err := gitworktree.ResolveRefSHA(ctx.Context(), repoPath, ref); err == nil {
		d.DeploySHA = sha
		d.DeployShortSHA = shortSHA(sha)
	}

	m, err := deploy.LoadAt(ctx.Context(), deploy.Runner(s.ghRun()), repoPath, ref, "")
	switch {
	case errors.Is(err, deploy.ErrNoManifest):
		// Not an error: most projects have no pipeline yet, and the page's job is
		// to say what to write and where.
		d.DeployNotConfigured = true
		return s.viewPage(ctx, "deploys", d)
	case err != nil:
		if d.Error == "" {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "deploys", d)
	}

	d.DeployEnvs = m.Environments
	d.DeployRows = buildDeployRows(m)
	d.DeployEnabled = s.cfg.ProjectDeployEnabled(project)
	d.DeployFeatureOn = s.cfg.FeatureDeploy()
	d.DeployEnvGated = map[string]bool{}
	for _, env := range m.Environments {
		// Gate strength drives the confirm styling: prod reads as dangerous,
		// dev does not.
		if envCfg, ok := s.cfg.ProjectDeployEnv(project, env); ok && envCfg.RequireCapability != "" {
			d.DeployEnvGated[env] = true
		}
	}
	if runs, err := s.deploys.Store().List(); err == nil {
		for _, r := range runs {
			if r.Project == project {
				d.DeployRecent = append(d.DeployRecent, r)
			}
			if len(d.DeployRecent) >= 20 {
				break
			}
		}
	}
	return s.viewPage(ctx, "deploys", d)
}

// buildDeployRows resolves every (service, environment) pair for display, so the
// board shows the same step lists a trigger would freeze onto a run.
func buildDeployRows(m *deploy.Manifest) []deployRow {
	rows := make([]deployRow, 0, len(m.Services))
	for _, name := range m.ServiceNames() {
		row := deployRow{Service: name, Dir: m.Dir(name), Cells: make([]deployCell, 0, len(m.Environments))}
		for _, env := range m.Environments {
			cell := deployCell{Env: env}
			steps, err := m.Resolve(name, env)
			if err != nil {
				// Keep the real reason: "narrowed away by envs" and "every step
				// filtered out for this environment" look identical on the board
				// otherwise, and the second one is an authoring mistake.
				cell.Reason = err.Error()
			} else {
				cell.Deployable = true
				cell.Steps = steps
			}
			row.Cells = append(row.Cells, cell)
		}
		rows = append(rows, row)
	}
	return rows
}

// pickedRepo resolves the repo selection from the combined "owner/repo" field
// the picker submits, falling back to the separate params a direct link carries.
//
// Parsing the combined value server-side is what lets the picker work with no
// JavaScript: the commits browser mirrors its select into hidden inputs from a
// script, so with scripting off its selection is silently ignored.
func pickedRepo(full, owner, repo string) (string, string) {
	if full = strings.TrimSpace(full); full != "" {
		if o, r, ok := strings.Cut(full, "/"); ok {
			return strings.TrimSpace(o), strings.TrimSpace(r)
		}
	}
	return strings.TrimSpace(owner), strings.TrimSpace(repo)
}
