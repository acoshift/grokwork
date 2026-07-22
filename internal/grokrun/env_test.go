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
	env, dropped := FilterChildEnv(base, false, nil)
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
	env, dropped := FilterChildEnv(base, true, nil)
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
