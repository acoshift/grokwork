package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// Deploys exposes the engine (main.go stops it on shutdown; tests seed it).
func (s *Server) Deploys() *deploy.Engine { return s.deploys }

// deployRedirect sends the operator back to a run or the board with a flash.
func (s *Server) deployRedirect(ctx *hime.Context, project, runID, okMsg string, err error) error {
	dest := "/projects/" + project + "/deploys"
	if runID != "" {
		dest += "/" + runID
	}
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	if len(q) > 0 {
		dest += "?" + q.Encode()
	}
	return ctx.Redirect(dest)
}

// postDeploy triggers a deploy.
func (s *Server) postDeploy(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	service := strings.TrimSpace(ctx.PostFormValue("service"))
	env := strings.TrimSpace(ctx.PostFormValue("env"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	owner, repo := pickedRepo(ctx.PostFormValue("repo_full"), ctx.PostFormValue("owner"), ctx.PostFormValue("repo"))

	repoPath, slug, err := s.deployRepoPath(ctx, project, root, owner, repo)
	if err != nil {
		return s.deployRedirect(ctx, project, "", "", err)
	}

	userID, _ := s.sessionIdentity(ctx)
	_, name := s.sessionDisplay(ctx)
	run, err := s.deploys.Trigger(ctx.Context(), deploy.TriggerRequest{
		Project: project, Repo: slug, RepoPath: repoPath,
		Service: service, Env: env, Ref: ref,
		ExpectSHA: strings.TrimSpace(ctx.PostFormValue("expect_sha")),
		Actor:     deploy.Actor{ID: userID, Name: name},
		Caps:      s.cfg.ResolveCapabilities(project, userID, nil),
	})
	s.auditAction(ctx, "deploy.trigger", err, map[string]any{
		"project": project, "service": service, "env": env, "ref": ref, "sha": run.SHA, "runId": run.ID,
	})
	if err != nil {
		return s.deployRedirect(ctx, project, "", "", err)
	}
	return s.deployRedirect(ctx, project, run.ID,
		fmt.Sprintf("Deploying %s to %s at %s", service, env, run.ShortSHA), nil)
}

// postDeployCancel stops a running deploy. Deliberately not rate limited: a
// runaway deploy must always be stoppable.
func (s *Server) postDeployCancel(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID := strings.TrimSpace(ctx.PathValue("runID"))
	err := s.deploys.Cancel(runID)
	s.auditAction(ctx, "deploy.cancel", err, map[string]any{"project": project, "runId": runID})
	return s.deployRedirect(ctx, project, runID, "Cancelling…", err)
}

// postDeployRedeploy replays a past run's frozen steps at its own commit.
//
// This is the whole rollback story for phase 1: runs are SHA-pinned and their
// steps are frozen, so re-running one is exactly reproducible.
func (s *Server) postDeployRedeploy(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID := strings.TrimSpace(ctx.PathValue("runID"))
	src, ok, err := s.deploys.Store().Get(runID)
	if err != nil || !ok || src.Project != project {
		return ctx.Status(http.StatusNotFound).Error("unknown deploy run")
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	owner, repo := "", ""
	if o, r, found := strings.Cut(src.Repo, "/"); found {
		owner, repo = o, r
	}
	repoPath, slug, err := s.deployRepoPath(ctx, project, root, owner, repo)
	if err != nil {
		return s.deployRedirect(ctx, project, runID, "", err)
	}
	userID, _ := s.sessionIdentity(ctx)
	_, name := s.sessionDisplay(ctx)
	run, err := s.deploys.Trigger(ctx.Context(), deploy.TriggerRequest{
		Project: project, Repo: slug, RepoPath: repoPath,
		Service: src.Service, Env: src.Env,
		Actor:      deploy.Actor{ID: userID, Name: name},
		Caps:       s.cfg.ResolveCapabilities(project, userID, nil),
		RedeployOf: runID,
	})
	s.auditAction(ctx, "deploy.redeploy", err, map[string]any{
		"project": project, "service": src.Service, "env": src.Env,
		"sha": src.SHA, "sourceRunId": runID, "runId": run.ID, "refCheck": run.RefCheck,
	})
	if err != nil {
		return s.deployRedirect(ctx, project, runID, "", err)
	}
	return s.deployRedirect(ctx, project, run.ID,
		fmt.Sprintf("Redeploying %s to %s at %s", src.Service, src.Env, src.ShortSHA), nil)
}

// deployRepoPath resolves the local checkout and the repo slug for a project.
func (s *Server) deployRepoPath(ctx *hime.Context, project, root, owner, repo string) (path, slug string, err error) {
	catalog, cErr := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if cErr != nil || len(catalog) == 0 {
		// A repo-less project deploys from its own checkout; mirror the commits
		// browser rather than hard-failing on the picker.
		if !gitworktree.IsRepo(root) {
			return "", "", errors.New("project path is not a git repository")
		}
		return root, "", nil
	}
	active, pErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pErr != nil {
		return "", "", pErr
	}
	resolved, rErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if rErr != nil {
		return "", "", rErr
	}
	return resolved, active.Owner + "/" + active.Repo, nil
}

// deployRunPage renders one run.
func (s *Server) deployRunPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID := strings.TrimSpace(ctx.PathValue("runID"))
	run, ok, err := s.deploys.Store().Get(runID)
	if err != nil || !ok || run.Project != project {
		return ctx.Status(http.StatusNotFound).Error("unknown deploy run")
	}
	d := s.basePage(ctx)
	d.Title = project + " · deploy " + run.ShortSHA
	d.IsDeploys = true
	d.Project = project
	d.DeployRun = &run
	d.CanDeploy = s.canDeploy(ctx, project, run.Env)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	d.Error = strings.TrimSpace(ctx.FormValue("err"))
	// Render the current tail inline. Leaving it to the first SSE tick would
	// show an empty log for up to two seconds, and forever when SSE is
	// unavailable — the page has to stand on its own.
	d.DeployLogStep = run.CurrentStep
	s.fillDeployLog(&d, run)
	return s.viewPage(ctx, "deploy_run", d)
}

// canDeploy reports whether the viewer may trigger for an environment.
func (s *Server) canDeploy(ctx *hime.Context, project, env string) bool {
	if !s.cfg.FeatureDeploy() {
		return false
	}
	userID, _ := s.sessionIdentity(ctx)
	caps := s.cfg.ResolveCapabilities(project, userID, nil)
	required := ""
	if envCfg, ok := s.cfg.ProjectDeployEnv(project, env); ok {
		required = envCfg.RequireCapability
	}
	return deploy.CapabilityAllows(caps, required)
}

// deployRunLog is the live tail fragment: it returns the bytes after the given
// offset plus the new offset, so a viewer arriving mid-run passes 0 and catches
// up in one request.
func (s *Server) deployRunLog(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID := strings.TrimSpace(ctx.PathValue("runID"))
	run, ok, err := s.deploys.Store().Get(runID)
	if err != nil || !ok || run.Project != project {
		return ctx.Status(http.StatusNotFound).Error("unknown deploy run")
	}
	idx := run.CurrentStep
	if raw := strings.TrimSpace(ctx.FormValue("step")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n < len(run.Steps) {
			idx = n
		}
	}
	var after int64
	if raw := strings.TrimSpace(ctx.FormValue("after")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	name := ""
	if idx < len(run.Steps) {
		name = run.Steps[idx].Name
	}
	_ = name
	_ = after
	d := s.basePage(ctx)
	d.Project = project
	d.DeployRun = &run
	d.DeployLogStep = idx
	s.fillDeployLog(&d, run)
	return s.viewFragment(ctx, "deploy_run", "deploy_log", d)
}

// fillDeployLog loads the bounded tail of the selected step.
func (s *Server) fillDeployLog(d *pageData, run deploy.Run) {
	idx := d.DeployLogStep
	if idx < 0 || idx >= len(run.Steps) {
		return
	}
	chunk, clipped, err := s.deploys.Store().ReadStepLogTail(run.ID, idx, run.Steps[idx].Name, deploy.LiveLogTailBytes)
	if err != nil {
		return
	}
	d.DeployLogChunk = string(chunk)
	d.DeployLogClipped = clipped
}

// deployRunLogRaw serves a whole step log as text.
func (s *Server) deployRunLogRaw(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	runID := strings.TrimSpace(ctx.PathValue("runID"))
	run, ok, err := s.deploys.Store().Get(runID)
	if err != nil || !ok || run.Project != project {
		return ctx.Status(http.StatusNotFound).Error("unknown deploy run")
	}
	idx := 0
	if raw := strings.TrimSpace(ctx.FormValue("step")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n < len(run.Steps) {
			idx = n
		}
	}
	name := ""
	if idx < len(run.Steps) {
		name = run.Steps[idx].Name
	}
	chunk, _, err := s.deploys.Store().ReadStepLog(runID, idx, name, 0)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error(err.Error())
	}
	w := ctx.ResponseWriter()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(chunk)
	return nil
}
