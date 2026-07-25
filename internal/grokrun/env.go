package grokrun

import (
	"os"
	"strings"
)

// DefaultEnvDenylistPrefixes are always stripped from agent children (Layer A / K26)
// except credentials a ChildEnvPolicy explicitly re-admits.
var DefaultEnvDenylistPrefixes = []string{
	"AWS_", "AZURE_", "GOOGLE_", "GCP_", "OPENAI_", "ANTHROPIC_", "XAI_",
	"DISCORD_", "GROK_WORK_",
	"NPM_TOKEN", "NODE_AUTH_TOKEN", "DOCKER_AUTH", "KUBECONFIG",
}

// ChildEnvPolicy decides which host credentials survive into an agent child.
// Everything is denied by default; each field re-admits one namespace.
type ChildEnvPolicy struct {
	// IncludeGHToken keeps GH_TOKEN / GITHUB_TOKEN so the agent can push and open PRs.
	IncludeGHToken bool
	// IncludeAnthropicEnv keeps ANTHROPIC_*, the claude CLI's own credential
	// namespace. An OAuth/keychain login authenticates without it, so this stays
	// off unless the host runs on an API key, a gateway, or a custom base URL.
	IncludeAnthropicEnv bool
	// ExtraDenylist is additional configured name prefixes to strip.
	ExtraDenylist []string
}

// FilterChildEnv builds a child environment from base (usually os.Environ())
// under pol. Returns the env slice and dropped variable names (for logging;
// never values).
func FilterChildEnv(base []string, pol ChildEnvPolicy) (env []string, dropped []string) {
	prefixes := make([]string, 0, len(DefaultEnvDenylistPrefixes)+len(pol.ExtraDenylist))
	prefixes = append(prefixes, DefaultEnvDenylistPrefixes...)
	prefixes = append(prefixes, pol.ExtraDenylist...)

	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if isGitHubTokenName(name) {
			if pol.IncludeGHToken {
				env = append(env, kv)
			} else {
				dropped = append(dropped, name)
			}
			continue
		}
		if isClaudeCredentialName(name) {
			if pol.IncludeAnthropicEnv {
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

// isClaudeCredentialName reports whether name carries Anthropic credentials.
//
// CLAUDE_CODE_OAUTH_TOKEN is matched by name rather than by a CLAUDE_ prefix on
// purpose: the rest of that namespace is behavior config (CLAUDE_CONFIG_DIR,
// CLAUDE_CODE_USE_BEDROCK, …), and stripping those would change how the CLI runs
// instead of protecting anything. Add new credential names here.
func isClaudeCredentialName(name string) bool {
	return strings.HasPrefix(name, "ANTHROPIC_") || name == "CLAUDE_CODE_OAUTH_TOKEN"
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
func ChildEnvFromOS(pol ChildEnvPolicy) (env []string, dropped []string) {
	return FilterChildEnv(os.Environ(), pol)
}
