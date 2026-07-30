package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
)

// DeployManifestOpts starts a session that authors the deploy pipeline file.
type DeployManifestOpts struct {
	Project string
	Actor   Actor
	// ManifestPath is where the file belongs (empty → the package default).
	ManifestPath string
	// Environments and EnvKeys describe the host's configured environments, so
	// the generated manifest matches what can actually be deployed. EnvKeys
	// carries variable NAMES only.
	Environments []string
	EnvKeys      map[string][]string
	// Existing is the current manifest, when there is one.
	Existing string
	// Requirements is the operator's free text from the deploys page.
	Requirements string
	// Model overrides the task model. Requires builder-class caps.
	Model string
}

// StartDeployManifestDraft creates a web-native unit that writes the pipeline
// file and ships it the normal way (branch, commit, PR — or direct, per project).
//
// This is an ordinary agent session, not a deploy run: it authors a file and
// opens a PR, so the manifest gets reviewed before it can govern a production
// deploy. Gating it on session-start rather than the deploy feature is what lets
// a project generate its pipeline *before* deploys are switched on.
func (b *Bot) StartDeployManifestDraft(opts DeployManifestOpts) (FixStartResult, error) {
	if b == nil {
		return FixStartResult{}, fmt.Errorf("bot is nil")
	}
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		return FixStartResult{}, ErrProjectRequired
	}
	cwd, ok := b.cfg.ProjectPath(project)
	if !ok || strings.TrimSpace(cwd) == "" {
		return FixStartResult{}, fmt.Errorf("unknown project %q", project)
	}
	// A model is only resolved and stamped when one was actually requested;
	// leaving it unstamped lets run start apply the config default, which is the
	// same contract StartWebTask uses.
	model := strings.TrimSpace(opts.Model)
	var cli config.AgentCLI
	if model != "" {
		if err := b.requireCanSelectModel(project, opts.Actor.ID); err != nil {
			return FixStartResult{}, err
		}
		var err error
		if cli, err = b.cfg.RequestedAgentCLI(model); err != nil {
			return FixStartResult{}, err
		}
	}

	path := strings.TrimSpace(opts.ManifestPath)
	if path == "" {
		path = deploy.DefaultManifestPath
	}
	prompt := deploy.BuildManifestPrompt(deploy.ManifestPromptOpts{
		Project:      project,
		Actor:        opts.Actor.DisplayName,
		ManifestPath: path,
		Environments: opts.Environments,
		EnvKeys:      opts.EnvKeys,
		Existing:     opts.Existing,
		Requirements: opts.Requirements,
	})

	// Web-native: there is no Discord thread to attach this to, and with no
	// thread name the goal is the only label the sessions list has.
	goal := deployManifestGoal(project, opts.Existing)
	return b.startWebNativeUnit(project, cwd, prompt, KindTask, opts.Actor, nil,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			if model == "" {
				return nil
			}
			return b.stampNewSessionCLI(unitID, cli)
		})
}

func deployManifestGoal(project, existing string) string {
	if strings.TrimSpace(existing) != "" {
		return "Update the deploy pipeline for " + project
	}
	return "Generate the deploy pipeline for " + project
}
