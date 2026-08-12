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
		"Pre-ship review (MANDATORY",
		"scrutinize",
		"SCRUTINIZE_VERDICT:",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	// Must not be Discord-exclusive wording only.
	if !strings.Contains(p, "shared machine") && !strings.Contains(p, "remote machine") {
		t.Fatalf("expected remote/shared machine wording: %s", p)
	}
	// Scrutinize is step 1 of the ship checklist (before commit/push/PR).
	if !strings.Contains(p, "1. "+scrutinizeBeforeShipStep) {
		t.Fatalf("scrutinize must be step 1 of ship checklist:\n%s", p)
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
		"Pre-ship review (MANDATORY",
		"SCRUTINIZE_VERDICT:",
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

func TestAttributionInShipPrefix(t *testing.T) {
	p := remoteWorkPromptPrefix("grok/discord/1") + attributionFooter("bob", "42", "https://discord.com/x")
	if !strings.Contains(p, "Prompter: bob") {
		t.Fatalf("missing attribution:\n%s", p)
	}
	if strings.Contains(p, "42") || strings.Contains(p, "https://discord.com/x") {
		t.Fatalf("must not include Discord id or thread URL:\n%s", p)
	}
}

func TestRemoteWorkPromptPrefixDirect(t *testing.T) {
	p := remoteWorkPromptPrefixMode("grok/discord/123", true, "")
	for _, want := range []string{
		"direct-to-primary",
		"Branch: grok/discord/123",
		"Do NOT open a pull request",
		"Do NOT push to main/master",
		"fast-forward integrate",
		"Pre-ship review (MANDATORY",
		"SCRUTINIZE_VERDICT:",
		"1. " + scrutinizeBeforeShipStep,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Include the PR URL") {
		t.Fatalf("direct mode must not require PR URL:\n%s", p)
	}
	// Mentions gh pr create only as forbidden for this repo, not as an instruction to run it.
	if strings.Contains(p, "4. Open a pull request with `gh pr create`") {
		t.Fatalf("direct mode must not instruct opening a PR:\n%s", p)
	}
	// No branch → falls back to PR-style wording even if direct flag set.
	p2 := remoteWorkPromptPrefixMode("", true, "")
	if !strings.Contains(p2, "gh pr create") {
		t.Fatalf("no-branch direct should fall back to PR wording:\n%s", p2)
	}
	if !strings.Contains(p2, "SCRUTINIZE_VERDICT:") {
		t.Fatalf("no-branch direct must still require scrutinize:\n%s", p2)
	}
}

func TestRemoteWorkPromptConfiguredPrimary(t *testing.T) {
	// PR mode: --base <primary>
	pr := remoteWorkPromptPrefixMode("grokwork/1", false, "prod")
	if !strings.Contains(pr, "gh pr create --base prod") {
		t.Fatalf("PR mode should pass --base prod:\n%s", pr)
	}
	if strings.Contains(pr, "HEAD:prod") {
		t.Fatalf("PR mode should not mention HEAD:prod:\n%s", pr)
	}
	// Direct mode: forbid push/commit to configured primary by name
	d := remoteWorkPromptPrefixMode("grokwork/1", true, "prod")
	for _, want := range []string{
		"Project primary branch: prod",
		"never commit to prod yourself",
		"Do NOT push to prod",
		"HEAD:prod",
		"onto prod",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("direct missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "Do NOT push to main/master") {
		t.Fatalf("configured primary should replace main/master forbid:\n%s", d)
	}
	// Empty primary keeps default PR wording without --base
	empty := remoteWorkPromptPrefixMode("grokwork/1", false, "")
	if strings.Contains(empty, "--base ") {
		t.Fatalf("empty primary must not inject --base:\n%s", empty)
	}
}

func TestInvestigatePromptDoesNotRequireScrutinizeShip(t *testing.T) {
	// Investigate never ships — do not load the pre-ship scrutinize contract there.
	p := investigatePromptPrefix("grok/discord/1", false)
	if strings.Contains(p, "SCRUTINIZE_VERDICT:") || strings.Contains(p, "Pre-ship review") {
		t.Fatalf("investigate mode must not inject pre-ship scrutinize:\n%s", p)
	}
}
