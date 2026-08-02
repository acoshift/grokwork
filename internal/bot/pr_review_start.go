package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// PRReviewOpts starts a new work unit from the web PR Review action.
// Always creates a new web-native session; never reuses one, never opens a Discord
// thread. Findings land in one PR comment — this path never files GitHub issues.
type PRReviewOpts struct {
	Project string
	Actor   Actor

	Owner  string
	Repo   string
	Number int
	Title  string
	URL    string
	State  string
	// Optional context the web pre-fetches for the prompt (all best-effort).
	HeadSHA      string
	HeadRef      string
	BaseRef      string
	Body         string // PR description
	Author       string
	Additions    int
	Deletions    int
	ChangedFiles int
	// Model overrides the configured review model for this session. Empty takes
	// config's review model (then the task model). Requires builder-class caps.
	Model string
}

// StartPRReview creates a workflow unit and enqueues an agentic PR-review task.
// The agent reads the PR with full tools and posts its verdict as a single PR
// comment via gh; the bot neither comments nor merges on its behalf.
func (b *Bot) StartPRReview(opts PRReviewOpts) (FixStartResult, error) {
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
	if opts.Number <= 0 || strings.TrimSpace(opts.Owner) == "" || strings.TrimSpace(opts.Repo) == "" {
		return FixStartResult{}, ErrInvalidPR
	}

	// Same gate and resolution order as the other dispatch cards: authorize a named
	// model first, then resolve it (or the configured review default), so a forged
	// form value fails the request instead of silently downgrading.
	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	// Always a web-native unit. Binds the PR so the detail page Sessions list can
	// show it, but stamps SessionKindPRReview so FindByPR (Address reuse) skips it
	// — a read-only review must not become the "continue" target for CI/review fixes.
	goal := prReviewGoal(opts)
	tracked := prReviewTrackedPR(opts)
	return b.startWebNativeUnit(project, cwd, BuildPRReviewPrompt(opts), KindTask, opts.Actor, nil,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			if err := b.bindPRReviewUnit(unitID, project, tracked, opts.Actor); err != nil {
				return err
			}
			return b.stampNewSessionCLI(unitID, cli)
		})
}

// bindPRReviewUnit binds the PR and stamps SessionKindPRReview on a unit that
// already has project/owner/goal from bindWebStartedSession.
func (b *Bot) bindPRReviewUnit(unitID, project string, tracked sessionstore.TrackedPR, actor Actor) error {
	if b.sessions == nil {
		return fmt.Errorf("sessions store nil")
	}
	_, ok, err := b.sessions.Patch(unitID, func(ent *sessionstore.Entry) {
		if ent.Project == "" {
			ent.Project = project
		}
		ent.SessionKind = sessionstore.SessionKindPRReview
		if actor.ID != "" {
			ensureSessionOwner(ent, actor.ID, actor.String())
		}
		ent.UpsertPR(tracked)
	})
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// bindWebStartedSession should have created the entry; if not, seed one.
	e := sessionstore.Entry{
		Project:     project,
		Origin:      SourceWeb,
		SessionKind: sessionstore.SessionKindPRReview,
	}
	if actor.ID != "" {
		ensureSessionOwner(&e, actor.ID, actor.String())
	}
	e.UpsertPR(tracked)
	return b.sessions.Set(unitID, e)
}

func prReviewTrackedPR(opts PRReviewOpts) sessionstore.TrackedPR {
	pr := sessionstore.TrackedPR{
		Owner:   strings.TrimSpace(opts.Owner),
		Repo:    strings.TrimSpace(opts.Repo),
		Number:  opts.Number,
		Title:   strings.TrimSpace(opts.Title),
		URL:     strings.TrimSpace(opts.URL),
		State:   strings.TrimSpace(opts.State),
		HeadSHA: strings.TrimSpace(opts.HeadSHA),
		HeadRef: strings.TrimSpace(opts.HeadRef),
	}
	if pr.State == "" {
		pr.State = "OPEN"
	}
	pr.FillOwnerRepoFromURL()
	return pr
}

func prReviewGoal(opts PRReviewOpts) string {
	sel := fmt.Sprintf("%s/%s#%d", strings.TrimSpace(opts.Owner), strings.TrimSpace(opts.Repo), opts.Number)
	goal := "Review " + sel
	if title := strings.TrimSpace(opts.Title); title != "" {
		goal += ": " + title
	}
	return clampGoal(goal)
}

// BuildPRReviewPrompt is the web-started agentic PR-review task body.
// Callers still prepend remoteWorkPromptPrefix at execute time.
// The single deliverable is one PR comment: no issues, no code changes, no merge.
func BuildPRReviewPrompt(opts PRReviewOpts) string {
	actorDisplay := strings.TrimSpace(opts.Actor.DisplayName)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	slug := owner + "/" + repo
	num := opts.Number
	prURL := strings.TrimSpace(opts.URL)
	if prURL == "" {
		prURL = fmt.Sprintf("https://github.com/%s/pull/%d", slug, num)
	}
	body := truncateRunes(strings.TrimSpace(opts.Body), fixPromptBodyMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (Review PR from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "Review pull request %s#%d and post your findings as **one comment on that pull request**.\n", slug, num)
	b.WriteString("You own the comment end-to-end (agentic): compose it and post it yourself with `gh`.\n")
	b.WriteString("Do **not** open GitHub issues for findings — the PR comment is the only deliverable.\n\n")

	b.WriteString("### Pull request\n")
	fmt.Fprintf(&b, "- Repo: %s\n", slug)
	fmt.Fprintf(&b, "- Number: #%d\n", num)
	if t := strings.TrimSpace(opts.Title); t != "" {
		fmt.Fprintf(&b, "- Title: %s\n", t)
	}
	fmt.Fprintf(&b, "- URL: %s\n", prURL)
	if s := strings.TrimSpace(opts.State); s != "" {
		fmt.Fprintf(&b, "- State: %s\n", s)
	}
	if a := strings.TrimSpace(opts.Author); a != "" {
		fmt.Fprintf(&b, "- Author: %s\n", a)
	}
	head := strings.TrimSpace(opts.HeadRef)
	base := strings.TrimSpace(opts.BaseRef)
	if head != "" && base != "" {
		fmt.Fprintf(&b, "- Branches: %s → %s\n", head, base)
	} else if head != "" {
		fmt.Fprintf(&b, "- Head branch: %s\n", head)
	} else if base != "" {
		fmt.Fprintf(&b, "- Base branch: %s\n", base)
	}
	if sha := strings.TrimSpace(opts.HeadSHA); sha != "" {
		fmt.Fprintf(&b, "- Head SHA: %s\n", sha)
	}
	if opts.ChangedFiles > 0 || opts.Additions > 0 || opts.Deletions > 0 {
		fmt.Fprintf(&b, "- Size: +%d −%d across %d files\n", opts.Additions, opts.Deletions, opts.ChangedFiles)
	}
	if body != "" {
		b.WriteString("\n### PR description\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	numStr := fmt.Sprint(num)
	prRef := numStr + " --repo " + slug
	b.WriteString(`
### How to review (multi-agent)
You are the orchestrator. Use subagents for depth and independent verification — do not do all review work alone when the change is large.

1. Read the change first: ` + "`gh pr diff " + prRef + "`" + ` for the patch and ` + "`gh pr view " + prRef + " --json files`" + ` (or ` + "`gh pr diff " + prRef + " --name-only`" + `) for the file list.
   The worktree is on the project's default branch, not the PR head. When you need full-file context rather than just hunks, fetch the head first:
   ` + "`git fetch origin pull/" + numStr + "/head`" + ` then read files at ` + "`FETCH_HEAD`" + ` (e.g. ` + "`git show FETCH_HEAD:<path>`" + `). Never push anything.
2. **Large change → fan out reviewers.** Treat as large if roughly any of:
   - ~15+ files changed, or
   - ~400+ lines added+deleted, or
   - several unrelated areas (packages/modules) in one PR.
   When large, spawn **multiple review subagents in parallel** (typically 2–6), split by directory/package/concern (e.g. auth vs API vs UI, or security-focused vs correctness-focused). Give each agent a clear file list / scope and the PR number.
   When small/medium, you may review yourself or use a single review subagent.
3. Each review agent should: inspect its scoped diff, look for correctness bugs, security issues, missing tests for risky changes, broken contracts, data loss, concurrency hazards; ignore style/naming/formatting unless it causes a real defect; never invent files or lines not seen.
4. **Always verify findings with a separate verifier subagent** (new agent, not a reviewer re-reading its own notes):
   - Pass candidate findings (claim, file:line, why it matters) plus the PR number.
   - Instruct the verifier to re-check the diff/code and mark each finding: **confirmed**, **downgrade** (with reason), or **reject** (false positive / not in this PR).
   - For large reviews with many candidates, you may spawn **multiple verifiers** in parallel (split by finding groups); still keep verification independent of the original reviewer.
5. Report only **confirmed** findings (or clearly real findings after you yourself re-check if a verifier is unavailable). Drop rejects; apply downgrades to severity.

### Posting the comment (agentic)
Post exactly one comment on the PR when the review is done:
- Write the markdown to a temp file, then ` + "`gh pr comment " + prRef + " --body-file <file>`" + ` — a body file survives backticks, ` + "`#`" + `, and newlines that inline ` + "`--body`" + ` would mangle.
- Structure it:
  - A one-line verdict (e.g. "No blocking issues found" / "2 blocking, 3 minor").
  - Findings ordered by severity, each as a bullet: **severity** — what is wrong, why it matters, suggested fix, and ` + "`file:line`" + ` when known.
  - Use ` + "`critical` / `high` / `medium` / `low` / `info`" + ` for severity.
  - A short closing note on what you did not cover (skipped areas, tests not run).
- Post the comment **even when the PR looks clean** — say so explicitly, so the requester sees the review ran.
- Prefer the highest-severity findings; skip nitpicks. Keep the comment readable (roughly ~15 findings max).

Rules:
- **Review only.** Do not edit files, do not commit, do not push, do not open a PR, and never merge. The PR comment is your only write.
- Do not open GitHub issues, and do not submit a GitHub approval/request-changes review — a plain comment only.
- In your final reply: note how many review/verifier agents you used, link the comment you posted, and give a short overall assessment.
`)
	return b.String()
}
