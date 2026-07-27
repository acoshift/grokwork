package grokrun

import (
	"slices"
	"strings"
	"testing"
)

func TestFilterChildEnvOmitGHToken(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"GH_TOKEN=secret",
		"GITHUB_TOKEN=secret2",
		"AWS_ACCESS_KEY_ID=x",
		"DISCORD_BOT_TOKEN=tok",
		"MY_APP=1",
	}
	env, dropped := FilterChildEnv(base, ChildEnvPolicy{})
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if isGitHubTokenName(name) || name == "DISCORD_BOT_TOKEN" || name == "AWS_ACCESS_KEY_ID" {
			t.Fatalf("should drop %s; env=%v dropped=%v", name, env, dropped)
		}
	}
	if !slices.Contains(dropped, "GH_TOKEN") || !slices.Contains(dropped, "GITHUB_TOKEN") {
		t.Fatalf("dropped=%v", dropped)
	}
	// PATH kept
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PATH missing: %v", env)
	}
}

func TestFilterChildEnvKeepGHToken(t *testing.T) {
	base := []string{"PATH=/bin", "GH_TOKEN=s", "AWS_SECRET_ACCESS_KEY=x", "DISCORD_TOKEN=d"}
	env, dropped := FilterChildEnv(base, ChildEnvPolicy{IncludeGHToken: true})
	hasGH := false
	for _, e := range env {
		if strings.HasPrefix(e, "GH_TOKEN=") {
			hasGH = true
		}
		if strings.HasPrefix(e, "AWS_") || strings.HasPrefix(e, "DISCORD_") {
			t.Fatalf("should still drop cloud/discord: %s", e)
		}
	}
	if !hasGH {
		t.Fatalf("expected GH_TOKEN kept; env=%v dropped=%v", env, dropped)
	}
}

// ANTHROPIC_* is a host credential like any other and is stripped by default.
// The claude CLI authenticates from its OAuth keychain, so this costs nothing
// until an operator explicitly opts into API-key or gateway auth.
func TestFilterChildEnvAnthropicGate(t *testing.T) {
	base := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-x", "ANTHROPIC_BASE_URL=https://gw"}

	env, dropped := FilterChildEnv(base, ChildEnvPolicy{})
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_") {
			t.Fatalf("ANTHROPIC_* must be stripped by default: %s", e)
		}
	}
	if !slices.Contains(dropped, "ANTHROPIC_API_KEY") {
		t.Fatalf("dropped=%v", dropped)
	}

	env, _ = FilterChildEnv(base, ChildEnvPolicy{IncludeAnthropicEnv: true})
	var kept int
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_") {
			kept++
		}
	}
	if kept != 2 {
		t.Fatalf("expected both ANTHROPIC_ vars kept, got %d: %v", kept, env)
	}
}

// CLAUDE_CODE_OAUTH_TOKEN is the other way a host authenticates claude, so it
// belongs behind the same gate — otherwise it reaches every child, grok included.
// The rest of the CLAUDE_ namespace is behavior config and must pass through, or
// stripping it would change how the CLI runs without protecting anything.
func TestFilterChildEnvClaudeOAuthTokenIsGated(t *testing.T) {
	base := []string{"CLAUDE_CODE_OAUTH_TOKEN=oat-secret", "CLAUDE_CONFIG_DIR=/home/u/.claude"}

	env, dropped := FilterChildEnv(base, ChildEnvPolicy{})
	if slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=oat-secret") {
		t.Fatalf("OAuth token must be stripped by default: %v", env)
	}
	if !slices.Contains(dropped, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("dropped=%v", dropped)
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/home/u/.claude") {
		t.Fatalf("non-credential CLAUDE_ vars must survive: %v", env)
	}

	env, _ = FilterChildEnv(base, ChildEnvPolicy{IncludeAnthropicEnv: true})
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=oat-secret") {
		t.Fatalf("opting in must re-admit the OAuth token: %v", env)
	}
}

// Every env fallback a webAuth login provider reads must be stripped from agent
// children. An operator who supplies webAuth.providers.github.clientSecret via
// GITHUB_CLIENT_SECRET (the name ValidateWebAuth tells them to use) would
// otherwise hand the OAuth app's client secret to every grok/claude run, whose
// output is streamed into Discord and written to data/history/ — one
// "@Grok what environment variables are set?" discloses it.
//
// The names are asserted literally rather than derived from config, so this test
// fails if either list moves: grokrun must not import internal/config.
func TestFilterChildEnvDropsWebAuthProviderSecrets(t *testing.T) {
	secrets := []string{
		"DISCORD_CLIENT_SECRET",
		"GROK_WORK_DISCORD_CLIENT_SECRET",
		"GOOGLE_CLIENT_SECRET",
		"GROK_WORK_GOOGLE_CLIENT_SECRET",
		"GITHUB_CLIENT_SECRET",
		"GROK_WORK_GITHUB_CLIENT_SECRET",
	}
	var base []string
	for _, name := range secrets {
		base = append(base, name+"=sentinel")
	}
	// IncludeGHToken is the permissive case: re-admitting the push token must not
	// re-admit a client secret that merely shares its prefix.
	base = append(base, "PATH=/bin", "GH_TOKEN=push", "GITHUB_TOKEN=push2")

	for _, pol := range []ChildEnvPolicy{{}, {IncludeGHToken: true}} {
		env, dropped := FilterChildEnv(base, pol)
		for _, name := range secrets {
			if slices.Contains(env, name+"=sentinel") {
				t.Fatalf("pol=%+v: %s leaked into agent child; env=%v", pol, name, env)
			}
			if !slices.Contains(dropped, name) {
				t.Fatalf("pol=%+v: %s not reported dropped; dropped=%v", pol, name, dropped)
			}
		}
		if !slices.Contains(env, "PATH=/bin") {
			t.Fatalf("pol=%+v: PATH must survive; env=%v", pol, env)
		}
	}

	// The deliberate re-admission at the heart of the fix's shape: GH_TOKEN and
	// GITHUB_TOKEN are still governed by IncludeGHToken, not by the secret check.
	env, _ := FilterChildEnv(base, ChildEnvPolicy{IncludeGHToken: true})
	if !slices.Contains(env, "GH_TOKEN=push") || !slices.Contains(env, "GITHUB_TOKEN=push2") {
		t.Fatalf("IncludeGHToken must still keep the push tokens; env=%v", env)
	}
}

// The gate is per-namespace: opting into claude credentials must not leak
// unrelated cloud or Discord secrets.
func TestFilterChildEnvAnthropicGateIsNarrow(t *testing.T) {
	base := []string{"ANTHROPIC_API_KEY=sk", "AWS_SECRET_ACCESS_KEY=x", "DISCORD_BOT_TOKEN=d", "OPENAI_API_KEY=o"}
	env, _ := FilterChildEnv(base, ChildEnvPolicy{IncludeAnthropicEnv: true})
	for _, e := range env {
		if !strings.HasPrefix(e, "ANTHROPIC_") {
			t.Fatalf("unexpected credential kept: %s", e)
		}
	}
}
