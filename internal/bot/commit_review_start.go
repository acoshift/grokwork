package bot

import (
	"fmt"
	"strings"
)

// CommitReviewOpts starts a new work unit from the web Commit Review action.
// Always creates a new web-native session; never reuses, never opens a Discord thread.
type CommitReviewOpts struct {
	Project  string
	Actor    Actor
	Owner    string
	Repo     string
	SHA      string
	ShortSHA string
	Subject  string
	Body     string // commit message body (optional)
	Author   string // "Name <email>" display
	Date     string // already formatted for humans
	// Model overrides the configured review model for this session. Empty takes
	// config's review model (then the task model). Requires builder-class caps.
	Model string
}

// StartCommitReview creates a workflow unit and enqueues an agentic commit-review task.
// The agent reviews the commit with full tools and opens GitHub issues itself (labels,
// commit references, etc.).
func (b *Bot) StartCommitReview(opts CommitReviewOpts) (FixStartResult, error) {
	if b == nil {
		return FixStartResult{}, fmt.Errorf("bot is nil")
	}
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		return FixStartResult{}, ErrProjectRequired
	}
	cwd, ok := b.cfg.ProjectPath(project)
	if !ok || strings.TrimSpace(cwd) == "" {
		return FixStartResult{}, fmt.Errorf("unknown project %q", project)
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	sha := strings.TrimSpace(opts.SHA)
	if owner == "" || repo == "" || sha == "" {
		return FixStartResult{}, fmt.Errorf("owner, repo, and commit sha are required")
	}

	if strings.TrimSpace(opts.Model) != "" {
		if err := b.requireCanSelectModel(project, opts.Actor.ID, nil); err != nil {
			return FixStartResult{}, err
		}
	}
	// Resolved and stamped here rather than left to run start: a review runs on the
	// review model, which the run path would otherwise re-resolve as the task model.
	cli, err := b.cfg.ReviewAgentCLI(opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	// Always a web-native unit, never a Discord thread. A review is read work that
	// ends in GitHub issues, so the running commentary belongs on the session page
	// that dispatched it — a Discord thread per reviewed commit is noise the reviewer
	// never asked for.
	goal := commitReviewGoal(opts)
	return b.startWebNativeUnit(project, cwd, BuildCommitReviewPrompt(opts), KindTask, opts.Actor,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			return b.stampNewSessionCLI(unitID, cli)
		})
}

func commitReviewGoal(opts CommitReviewOpts) string {
	short := strings.TrimSpace(opts.ShortSHA)
	if short == "" {
		sha := strings.TrimSpace(opts.SHA)
		if len(sha) >= 7 {
			short = sha[:7]
		} else {
			short = sha
		}
	}
	sub := strings.TrimSpace(opts.Subject)
	if sub == "" {
		return "Review commit " + short
	}
	g := "Review " + short + ": " + sub
	if len(g) > 120 {
		g = g[:117] + "…"
	}
	return g
}

// BuildCommitReviewPrompt is the web-started agentic commit-review task body.
// Callers still prepend remoteWorkPromptPrefix at execute time.
// Grok owns issue creation via gh (labels, body, commit links) — the bot does not file issues.
func BuildCommitReviewPrompt(opts CommitReviewOpts) string {
	actorDisplay := strings.TrimSpace(opts.Actor.DisplayName)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	sha := strings.TrimSpace(opts.SHA)
	short := strings.TrimSpace(opts.ShortSHA)
	if short == "" && len(sha) >= 7 {
		short = sha[:7]
	}
	subject := strings.TrimSpace(opts.Subject)
	body := truncateRunes(strings.TrimSpace(opts.Body), fixPromptBodyMaxRunes)
	commitURL := ""
	if owner != "" && repo != "" && sha != "" {
		commitURL = fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, sha)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	b.WriteString("Review a single git commit and open GitHub issues for real findings.\n")
	b.WriteString("You own issue creation end-to-end (agentic): use `gh issue create`, labels, and body yourself.\n")
	b.WriteString("The bot will not file issues for you.\n\n")

	b.WriteString("### Commit\n")
	fmt.Fprintf(&b, "- Repo: %s/%s\n", owner, repo)
	fmt.Fprintf(&b, "- SHA: %s", sha)
	if short != "" && short != sha {
		fmt.Fprintf(&b, " (`%s`)", short)
	}
	b.WriteByte('\n')
	if subject != "" {
		fmt.Fprintf(&b, "- Subject: %s\n", subject)
	}
	if a := strings.TrimSpace(opts.Author); a != "" {
		fmt.Fprintf(&b, "- Author: %s\n", a)
	}
	if d := strings.TrimSpace(opts.Date); d != "" {
		fmt.Fprintf(&b, "- Date: %s\n", d)
	}
	if commitURL != "" {
		fmt.Fprintf(&b, "- URL: %s\n", commitURL)
	}
	if body != "" {
		b.WriteString("\n### Commit message body\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	b.WriteString(`
### How to review (multi-agent)
You are the orchestrator. Use subagents for depth and independent verification — do not do all review work alone when the change is large.

1. Size the commit first: ` + "`git show --stat " + sha + "`" + ` and ` + "`git show --numstat " + sha + "`" + `.
2. **Large change → fan out reviewers.** Treat as large if roughly any of:
   - ~15+ files changed, or
   - ~400+ lines added+deleted, or
   - several unrelated areas (packages/modules) in one commit.
   When large, spawn **multiple review subagents in parallel** (typically 2–6), split by directory/package/concern (e.g. auth vs API vs UI, or security-focused vs correctness-focused). Give each agent a clear file list / scope and the full SHA.
   When small/medium, you may review yourself or use a single review subagent.
3. Each review agent should: inspect its scoped diff (` + "`git show " + sha + " -- <paths>`" + `), look for correctness bugs, security issues, missing tests for risky changes, broken contracts, data loss, concurrency hazards; ignore style/naming/formatting unless it causes a real defect; never invent files or lines not seen.
4. **Always verify findings with a separate verifier subagent** (new agent, not a reviewer re-reading its own notes):
   - Pass candidate findings (claim, file:line, why it matters) plus the commit SHA.
   - Instruct the verifier to re-check the diff/code and mark each finding: **confirmed**, **downgrade** (with reason), or **reject** (false positive / not in this commit).
   - For large reviews with many candidates, you may spawn **multiple verifiers** in parallel (split by finding groups); still keep verification independent of the original reviewer.
5. File issues only for **confirmed** findings (or clearly real findings after you yourself re-check if a verifier is unavailable). Drop rejects; apply downgrades to severity.

### Filing issues (agentic)
For each confirmed finding, open a GitHub issue yourself with ` + "`gh`" + `:
- Title: short, specific (prefix with ` + "`[review/" + short + "]`" + ` when helpful).
- Body (markdown): problem, why it matters, suggested fix, file:line when known.
  Always reference the commit (link ` + commitURL + ` and mention SHA ` + "`" + short + "`" + `).
  Mention related commits/PRs/issues when relevant.
- Labels: create if missing, then apply. Prefer:
  - ` + "`commit-review`" + `
  - ` + "`severity:critical|high|medium|low|info`" + `
  - optional domain labels (e.g. security, performance) when they already exist or you create them.
- Repo: ` + owner + `/` + repo + ` (` + "`gh issue create --repo " + owner + "/" + repo + "`" + `).

Rules:
- Prefer highest-severity findings; skip nitpicks. Cap at ~15 issues.
- If the commit looks fine after review+verify, open zero issues and say so clearly.
- Do not merge anything. Code fixes are optional: only implement a fix in this worktree if it is clearly warranted; otherwise filing issues is enough.
- In your final reply: note how many review/verifier agents you used, list issue URLs (or state that none were filed), and give a short overall assessment.
`)
	return b.String()
}
