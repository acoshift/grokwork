package bot

import (
	"fmt"
	"strings"
)

const fixPromptBodyMaxRunes = 12_000

// BuildGitHubFixPrompt is the Fix-with-Grok task body for a GitHub issue (web).
// Callers still prepend remoteWorkPromptPrefix + issueBindingPrompt at execute time;
// this fragment carries the user-facing fix task and do-not-merge contract.
func BuildGitHubFixPrompt(actorDisplay, owner, repo string, number int, title, url, body string) string {
	actorDisplay = strings.TrimSpace(actorDisplay)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	title = strings.TrimSpace(title)
	url = strings.TrimSpace(url)
	if url == "" && owner != "" && repo != "" && number > 0 {
		url = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, number)
	}
	body = truncateRunes(strings.TrimSpace(body), fixPromptBodyMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "Fix GitHub issue %s/%s#%d: %s\n", owner, repo, number, title)
	if url != "" {
		fmt.Fprintf(&b, "URL: %s\n", url)
	}
	b.WriteString("\n### Issue body\n")
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	} else {
		b.WriteString("(no body)\n")
	}
	b.WriteString("\nImplement the fix in this worktree, commit, push, and open/update a PR.\n")
	fmt.Fprintf(&b, "Use Fixes %s/%s#%d in the PR body. Do not merge.\n", owner, repo, number)
	return b.String()
}

// BuildLinearFixPrompt is the Fix-with-Grok task body for a Linear issue (web).
func BuildLinearFixPrompt(actorDisplay, identifier, title, url, state, description string) string {
	actorDisplay = strings.TrimSpace(actorDisplay)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	identifier = strings.TrimSpace(identifier)
	title = strings.TrimSpace(title)
	url = strings.TrimSpace(url)
	state = strings.TrimSpace(state)
	description = truncateRunes(strings.TrimSpace(description), fixPromptBodyMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "Fix Linear issue %s: %s\n", identifier, title)
	if url != "" {
		fmt.Fprintf(&b, "URL: %s\n", url)
	}
	if state != "" {
		fmt.Fprintf(&b, "State: %s\n", state)
	}
	b.WriteString("\n### Description\n")
	if description != "" {
		b.WriteString(description)
		b.WriteString("\n")
	} else {
		b.WriteString("(no description)\n")
	}
	b.WriteString("\nImplement the fix in this worktree, commit, push, and open/update a PR.\n")
	fmt.Fprintf(&b, "Put %s in the PR title and body (Fixes %s) so Linear's\n", identifier, identifier)
	b.WriteString("GitHub integration can move state. Do not call Linear issueUpdate. Do not merge.\n")
	return b.String()
}
