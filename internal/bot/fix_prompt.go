package bot

import (
	"fmt"
	"strings"
)

const fixPromptBodyMaxRunes = 12_000

// extraInstructionsHeading marks the optional user note. Empty extra never emits it.
const extraInstructionsHeading = "### Additional instructions\n"

// appendExtraInstructions appends a trimmed user note after the canned prompt.
// Whitespace-only extra returns prompt unchanged. Over-long text is shortened
// to fixPromptBodyMaxRunes; the canned prompt is never truncated. The note is
// copied as typed — #, ?, and & stay literal.
func appendExtraInstructions(prompt, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return prompt
	}
	extra = truncateRunes(extra, fixPromptBodyMaxRunes)
	return prompt + "\n" + extraInstructionsHeading + extra + "\n"
}

// BuildGitHubFixPrompt is the Fix-with-Grok task body for a GitHub issue (web).
// Callers still prepend remoteWorkPromptPrefix + issueBindingPrompt at execute time;
// this fragment carries the user-facing fix task and ship contract.
// direct selects No-PR wording (commit + host ship) vs open/update a PR.
// extra is optional user instructions; empty leaves the canned body unchanged.
func BuildGitHubFixPrompt(actorDisplay, owner, repo string, number int, title, url, body string, direct bool, extra string) string {
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
	b.WriteString(fixPromptShipSteps(direct, fmt.Sprintf("Fixes %s/%s#%d", owner, repo, number)))
	return appendExtraInstructions(b.String(), extra)
}

// BuildGitHubImplementPrompt is the issue-detail Implement task. A new session
// starts with `/goal` so grok's handle_prompt intercepts GoalSet; reuse omits
// it so a live session is not told the goal is already set. Callers still
// prepend remoteWorkPromptPrefix (folded after the /goal line at execute time).
func BuildGitHubImplementPrompt(actorDisplay, owner, repo string, number int, title, url, body string, direct, reuse bool) string {
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
	sel := fmt.Sprintf("%s/%s#%d", owner, repo, number)

	var b strings.Builder
	if !reuse {
		fmt.Fprintf(&b, "/goal Implement the plan in GitHub issue %s", sel)
		if title != "" {
			fmt.Fprintf(&b, ": %s", title)
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "\n## Task (started from web by %s)\n", actorDisplay)
		b.WriteString("Implement this issue's plan end-to-end. The issue body — including any Breakdown tasklist — is the plan. Work until the plan is done.\n")
	} else {
		fmt.Fprintf(&b, "## Task (continued from web by %s)\n", actorDisplay)
		fmt.Fprintf(&b, "Continue implementing the plan in GitHub issue %s", sel)
		if title != "" {
			fmt.Fprintf(&b, ": %s", title)
		}
		b.WriteByte('\n')
		b.WriteString("Do not start a new /goal; finish remaining work on this session.\n")
	}
	if url != "" {
		fmt.Fprintf(&b, "URL: %s\n", url)
	}
	b.WriteString("\n### Plan\n")
	if body != "" {
		b.WriteString(body)
		b.WriteByte('\n')
	} else {
		b.WriteString("(no body)\n")
	}
	b.WriteString(fixPromptShipSteps(direct, "Fixes "+sel))
	return b.String()
}

// BuildLinearFixPrompt is the Fix-with-Grok task body for a Linear issue (web).
// extra is optional user instructions; empty leaves the canned body unchanged.
func BuildLinearFixPrompt(actorDisplay, identifier, title, url, state, description string, direct bool, extra string) string {
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
	b.WriteString(fixPromptShipSteps(direct, "Fixes "+identifier))
	b.WriteString("Use grokwork MCP linear_get_issue for more detail. Do not call Linear's HTTP API. Do not call Linear issueUpdate.\n")
	if !direct {
		b.WriteString("Put the identifier in the PR title so Linear's GitHub integration can move state.\n")
	}
	return appendExtraInstructions(b.String(), extra)
}

// BuildClickUpFixPrompt is the Fix-with-Grok task body for a ClickUp task (web).
// extra is optional user instructions; empty leaves the canned body unchanged.
func BuildClickUpFixPrompt(actorDisplay, display, title, url, state, description string, direct bool, extra string) string {
	actorDisplay = strings.TrimSpace(actorDisplay)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	display = strings.TrimSpace(display)
	title = strings.TrimSpace(title)
	url = strings.TrimSpace(url)
	state = strings.TrimSpace(state)
	description = truncateRunes(strings.TrimSpace(description), fixPromptBodyMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "Fix ClickUp task %s: %s\n", display, title)
	if url != "" {
		fmt.Fprintf(&b, "URL: %s\n", url)
	}
	if state != "" {
		fmt.Fprintf(&b, "Status: %s\n", state)
	}
	b.WriteString("\n### Description\n")
	if description != "" {
		b.WriteString(description)
		b.WriteString("\n")
	} else {
		b.WriteString("(no description)\n")
	}
	b.WriteString(fixPromptShipSteps(direct, "Fixes "+display))
	b.WriteString("Use grokwork MCP clickup_get_task for more detail. Do not call ClickUp's HTTP API.\n")
	return appendExtraInstructions(b.String(), extra)
}

func fixPromptShipSteps(direct bool, fixesLine string) string {
	fixesLine = strings.TrimSpace(fixesLine)
	if direct {
		var b strings.Builder
		b.WriteString("\nImplement the fix in this worktree and commit on this branch. Do not open a PR for this project — the host bot ships to primary after a successful run.\n")
		if fixesLine != "" {
			b.WriteString("Put this exact line in the commit message body: ")
			b.WriteString(fixesLine)
			b.WriteString("\n")
		}
		return b.String()
	}
	var b strings.Builder
	b.WriteString("\nImplement the fix in this worktree, commit, push, and open/update a PR.\n")
	if fixesLine != "" {
		b.WriteString("Use ")
		b.WriteString(fixesLine)
		b.WriteString(" in the PR body. Do not merge.\n")
	} else {
		b.WriteString("Do not merge.\n")
	}
	return b.String()
}
