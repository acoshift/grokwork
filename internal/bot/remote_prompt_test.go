package bot

import (
	"strings"
	"testing"
)

func TestRemoteWorkPromptPrefixWorktree(t *testing.T) {
	p := remoteWorkPromptPrefix("grok/discord/123")
	for _, want := range []string{
		"workflow unit",
		"Branch: grok/discord/123",
		"git push",
		"gh pr create",
		"Do not merge",
		"PR URL",
		"Do not leave work as local-only commits",
		"~/Documents",
		"Do NOT scan or search the user's home directory",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	// Must not be Discord-exclusive wording only.
	if !strings.Contains(p, "shared machine") && !strings.Contains(p, "remote machine") {
		t.Fatalf("expected remote/shared machine wording: %s", p)
	}
}

func TestRemoteWorkPromptPrefixNoWorktree(t *testing.T) {
	p := remoteWorkPromptPrefix("")
	for _, want := range []string{
		"workflow unit",
		"feature branch",
		"gh pr create",
		"PR URL",
		"~/Documents",
		"Do NOT scan or search the user's home directory",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Branch: ") {
		t.Fatalf("unexpected worktree branch line: %s", p)
	}
}

func TestIssueBindingPromptInPrefixChain(t *testing.T) {
	// remote prefix + issue binding is how executeTask assembles the prompt head.
	head := remoteWorkPromptPrefix("grok/discord/1") + issueBindingPrompt(nil)
	if !strings.Contains(head, "gh pr create") {
		t.Fatalf("missing pr create: %s", head)
	}
	// empty issues add nothing
	if strings.Contains(head, "Linked GitHub issues") {
		t.Fatalf("unexpected issues block: %s", head)
	}
}

func TestRemoteWorkPromptPrefixDirect(t *testing.T) {
	p := remoteWorkPromptPrefixMode("grok/discord/123", true)
	for _, want := range []string{
		"direct-to-primary",
		"Branch: grok/discord/123",
		"Do NOT open a pull request",
		"Do NOT push to main/master",
		"fast-forward integrate",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Include the PR URL") {
		t.Fatalf("direct mode must not require PR URL:\n%s", p)
	}
	// Mentions gh pr create only as forbidden for this repo, not as an instruction to run it.
	if strings.Contains(p, "3. Open a pull request with `gh pr create`") {
		t.Fatalf("direct mode must not instruct opening a PR:\n%s", p)
	}
	// No branch → falls back to PR-style wording even if direct flag set.
	p2 := remoteWorkPromptPrefixMode("", true)
	if !strings.Contains(p2, "gh pr create") {
		t.Fatalf("no-branch direct should fall back to PR wording:\n%s", p2)
	}
}
