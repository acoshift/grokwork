package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// postDeployGenerate starts an agent session that writes the pipeline file.
//
// Gated on startSessions, NOT on the deploy feature: this authors a file and
// opens a PR rather than touching an environment, and a project needs its
// pipeline written before deploys can sensibly be switched on.
func (s *Server) postDeployGenerate(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	// Same money/risk gate as every other session start.
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).CanShip() {
		return s.deployRedirect(ctx, project, "", "",
			errors.New("you do not have permission to start sessions for this project"))
	}
	if err := s.checkStartRate(ctx); err != nil {
		return s.deployRedirect(ctx, project, "", "", err)
	}

	owner, repo := pickedRepo(ctx.PostFormValue("repo_full"), ctx.PostFormValue("owner"), ctx.PostFormValue("repo"))
	repoPath, _, err := s.deployRepoPath(ctx, project, root, owner, repo)
	if err != nil {
		return s.deployRedirect(ctx, project, "", "", err)
	}

	// Read the current manifest so the agent edits rather than replaces. Read
	// from the primary tip, the same revision the board displays.
	existing := ""
	manifestPath := s.cfg.ProjectDeployManifestPath(project)
	ref := gitworktree.PrimaryStartRef(ctx.Context(), repoPath)
	if raw, err := deploy.ReadRawManifestAt(ctx.Context(), deploy.Runner(s.ghRun()), repoPath, ref, manifestPath); err == nil {
		existing = raw
	}

	envs := s.cfg.ProjectDeployEnvNames(project)
	envKeys := map[string][]string{}
	for _, env := range envs {
		if cfg, ok := s.cfg.ProjectDeployEnv(project, env); ok {
			// Names only. Values are credentials and never enter a prompt.
			keys := make([]string, 0, len(cfg.Env))
			for k := range cfg.Env {
				keys = append(keys, k)
			}
			envKeys[env] = keys
		}
	}

	res, startErr := s.bot.StartDeployManifestDraft(bot.DeployManifestOpts{
		Project:      project,
		Actor:        s.fixActor(ctx),
		ManifestPath: manifestPath,
		Environments: envs,
		EnvKeys:      envKeys,
		Existing:     existing,
		Requirements: ctx.PostFormValue("requirements"),
		Model:        strings.TrimSpace(ctx.PostFormValue("model")),
	})
	s.auditAction(ctx, audit.ActionSessionStart, startErr, map[string]any{
		"project": project, "kind": "deploy.manifest", "threadId": res.ThreadID,
		"existing": existing != "", "envs": len(envs),
	})
	if startErr != nil {
		return s.deployRedirect(ctx, project, "", "", startErr)
	}
	return s.sessionRedirect(ctx, res.ThreadID, "Started a session to write the deploy pipeline", "")
}
