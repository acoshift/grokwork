package web

import (
	"fmt"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
)

// projectConfigDeployPage is the per-project Deploy settings tab: environments,
// their gates, and their credentials. The pipeline itself is not editable here —
// it lives in the repo.
func (s *Server) projectConfigDeployPage(ctx *hime.Context) error {
	return s.projectConfigTab(ctx, "deploy", "project_config_deploy", func(d *pageData) {
		d.DeployCapabilityNames = config.ValidDeployCapabilities
		d.DeployFeatureOn = s.cfg.FeatureDeploy()
	})
}

func (s *Server) setProjectDeployEnabled(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	enabled := checkboxOn(ctx.PostFormValue("enabled"))
	err := s.cfg.SetProjectDeployEnabled(name, enabled)
	s.auditAction(ctx, "config.set_project_deploy_enabled", err, map[string]any{"name": name, "enabled": enabled})
	msg := "Deploys disabled for " + name
	if enabled {
		msg = "Deploys enabled for " + name
	}
	return s.projectConfigTabRedirect(ctx, name, "deploy", msg, err)
}

func (s *Server) setProjectDeployEnv(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	env := strings.TrimSpace(ctx.PostFormValue("env"))
	pol := config.DeployEnvPolicy{
		RequireCapability: strings.TrimSpace(ctx.PostFormValue("requireCapability")),
		AllowedRefs:       splitLines(ctx.PostFormValue("allowedRefs")),
	}
	if raw := strings.TrimSpace(ctx.PostFormValue("stepTimeoutMax")); raw != "" {
		ms, err := parseTimeoutMs(raw)
		if err != nil {
			return s.projectConfigTabRedirect(ctx, name, "deploy", "", fmt.Errorf("step timeout ceiling: %w", err))
		}
		pol.StepTimeoutMaxMs = &ms
	}
	err := s.cfg.SetProjectDeployEnvPolicy(name, env, pol)
	// Detail records the policy, never a credential.
	s.auditAction(ctx, "config.set_project_deploy_env", err, map[string]any{
		"name": name, "env": env, "requireCapability": pol.RequireCapability, "refs": len(pol.AllowedRefs),
	})
	return s.projectConfigTabRedirect(ctx, name, "deploy", fmt.Sprintf("Saved environment %q", env), err)
}

func (s *Server) removeProjectDeployEnv(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	env := strings.TrimSpace(ctx.PostFormValue("env"))
	err := s.cfg.RemoveProjectDeployEnv(name, env)
	s.auditAction(ctx, "config.remove_project_deploy_env", err, map[string]any{"name": name, "env": env})
	return s.projectConfigTabRedirect(ctx, name, "deploy", fmt.Sprintf("Removed environment %q", env), err)
}

func (s *Server) setProjectDeployEnvVar(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	env := strings.TrimSpace(ctx.PostFormValue("env"))
	key := strings.TrimSpace(ctx.PostFormValue("key"))
	// Not trimmed: a credential may legitimately begin or end with whitespace,
	// and silently altering one produces a failure nobody can explain.
	value := ctx.PostFormValue("value")
	secret := checkboxOn(ctx.PostFormValue("secret"))
	err := s.cfg.SetProjectDeployEnvVar(name, env, key, value, secret)
	// Detail carries the variable NAME and whether it is redacted — never the value.
	s.auditAction(ctx, "config.set_project_deploy_env_var", err, map[string]any{
		"name": name, "env": env, "key": key, "secret": secret,
	})
	return s.projectConfigTabRedirect(ctx, name, "deploy", fmt.Sprintf("Saved %s for %s", key, env), err)
}

func (s *Server) removeProjectDeployEnvVar(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	env := strings.TrimSpace(ctx.PostFormValue("env"))
	key := strings.TrimSpace(ctx.PostFormValue("key"))
	err := s.cfg.RemoveProjectDeployEnvVar(name, env, key)
	s.auditAction(ctx, "config.remove_project_deploy_env_var", err, map[string]any{
		"name": name, "env": env, "key": key,
	})
	return s.projectConfigTabRedirect(ctx, name, "deploy", fmt.Sprintf("Removed %s from %s", key, env), err)
}

// splitLines is the shared "one entry per line, # comments skipped" reader used
// by the textarea list fields.
func splitLines(raw string) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// checkboxOn matches the form convention used across the config handlers.
func checkboxOn(v string) bool {
	return v == "1" || strings.EqualFold(v, "on")
}
