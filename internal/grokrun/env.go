package grokrun

import (
	"os"
	"strings"
)

// DefaultEnvDenylistPrefixes are always stripped from Grok children (Layer A / K26)
// except GitHub tokens when includeGHToken is true.
var DefaultEnvDenylistPrefixes = []string{
	"AWS_", "AZURE_", "GOOGLE_", "GCP_", "OPENAI_", "ANTHROPIC_", "XAI_",
	"DISCORD_", "GROK_WORK_",
	"NPM_TOKEN", "NODE_AUTH_TOKEN", "DOCKER_AUTH", "KUBECONFIG",
}

// FilterChildEnv builds a child environment from base (usually os.Environ()).
// When includeGHToken is false, GH_TOKEN / GITHUB_TOKEN (and similar) are omitted.
// extraDenylist prefixes are also stripped.
// Returns the env slice and dropped variable names (for logging; never values).
func FilterChildEnv(base []string, includeGHToken bool, extraDenylist []string) (env []string, dropped []string) {
	prefixes := make([]string, 0, len(DefaultEnvDenylistPrefixes)+len(extraDenylist))
	prefixes = append(prefixes, DefaultEnvDenylistPrefixes...)
	prefixes = append(prefixes, extraDenylist...)

	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if isGitHubTokenName(name) {
			if includeGHToken {
				env = append(env, kv)
			} else {
				dropped = append(dropped, name)
			}
			continue
		}
		if name == "DISCORD_BOT_TOKEN" || name == "DISCORD_TOKEN" || name == "DISCORD_CLIENT_SECRET" {
			dropped = append(dropped, name)
			continue
		}
		if matchesDenylist(name, prefixes) {
			dropped = append(dropped, name)
			continue
		}
		env = append(env, kv)
	}
	return env, dropped
}

func isGitHubTokenName(name string) bool {
	switch name {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
		return true
	default:
		return false
	}
}

func matchesDenylist(name string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if name == p {
			return true
		}
		if strings.HasSuffix(p, "_") && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ChildEnvFromOS is a convenience for tests and callers.
func ChildEnvFromOS(includeGHToken bool, extraDenylist []string) (env []string, dropped []string) {
	return FilterChildEnv(os.Environ(), includeGHToken, extraDenylist)
}
