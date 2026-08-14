package grokrun

import (
	"os"
	"strings"
)

// DefaultEnvDenylistPrefixes are always stripped from agent children (Layer A / K26)
// except credentials a ChildEnvPolicy explicitly re-admits.
var DefaultEnvDenylistPrefixes = []string{
	"AWS_", "AZURE_", "GOOGLE_", "GCP_", "OPENAI_", "ANTHROPIC_", "XAI_",
	"DISCORD_", "GROK_WORK_", "CLICKUP_", "LINEAR_",
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
	// IncludeAgentToken keeps GROKWORK_AGENT_TOKEN for the in-session MCP bridge.
	// The name is intentionally not under GROK_WORK_* (that prefix is denylisted).
	IncludeAgentToken bool
	// ExtraDenylist is additional configured name prefixes to strip.
	ExtraDenylist []string
}

// AgentTokenEnv is the child env var name for the session-bound agent API token.
const AgentTokenEnv = "GROKWORK_AGENT_TOKEN"

// AgentSockEnv is the Unix socket path for the in-session MCP bridge.
const AgentSockEnv = "GROKWORK_AGENT_SOCK"

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
		if name == AgentTokenEnv {
			if pol.IncludeAgentToken {
				env = append(env, kv)
			} else {
				dropped = append(dropped, name)
			}
			continue
		}
		if isHostSecretName(name) {
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

// isHostSecretName reports whether name carries a grokwork host credential that
// no agent child may ever see. These are matched by exact name rather than by a
// prefix because their namespaces are shared with variables that must pass
// through: a "GITHUB_" denylist prefix would swallow GH_TOKEN/GITHUB_TOKEN,
// which IncludeGHToken deliberately re-admits so the agent can push and open PRs.
//
// INVARIANT: every env fallback a webAuth login provider reads must be listed
// here. config.OAuthProviderCreds resolves each provider's secret from config
// first and then from the environment (see internal/config/webauth_providers.go);
// a name that appears there and not here is handed verbatim to every coding
// agent, whose output is streamed into Discord and persisted to data/history/.
// GOOGLE_CLIENT_SECRET and GROK_WORK_* are already covered by the GOOGLE_ and
// GROK_WORK_ denylist prefixes; TestFilterChildEnvDropsWebAuthProviderSecrets
// pins the whole set so the two lists cannot drift apart again.
func isHostSecretName(name string) bool {
	switch name {
	case "DISCORD_BOT_TOKEN", "DISCORD_TOKEN", "DISCORD_CLIENT_SECRET", "GITHUB_CLIENT_SECRET":
		return true
	default:
		return false
	}
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
