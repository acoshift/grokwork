package bot

import (
	"strings"
	"testing"
)

func TestRemoteWorkPromptPrefixWorktree(t *testing.T) {
	p := remoteWorkPromptPrefix("grok/discord/123")
	for _, want := range []string{
		"remotely via Discord",
		"Branch: grok/discord/123",
		"git push",
		"gh pr create",
		"Do not merge",
		"PR URL",
		"Do not leave work as local-only commits",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
}

func TestRemoteWorkPromptPrefixNoWorktree(t *testing.T) {
	p := remoteWorkPromptPrefix("")
	for _, want := range []string{
		"remotely via Discord",
		"feature branch",
		"gh pr create",
		"PR URL",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Branch: ") {
		t.Fatalf("unexpected worktree branch line: %s", p)
	}
}
