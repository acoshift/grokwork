package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// CommitReviewOpts starts a new work unit from the web Commit Review action.
// Always creates a new Discord thread (or web-native unit); never reuses.
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
}

// StartCommitReview creates a workflow unit and enqueues an agentic commit-review task.
// Grok reviews the commit with full tools and opens GitHub issues itself (labels, commit
// references, etc.). Prefer Discord when the gateway/threadAPI is available.
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

	prompt := BuildCommitReviewPrompt(opts)
	goal := commitReviewGoal(opts)

	return b.startCommitReviewCreate(project, cwd, prompt, goal, opts)
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

func (b *Bot) startCommitReviewCreate(project, cwd, prompt, goal string, opts CommitReviewOpts) (FixStartResult, error) {
	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if err != nil {
			return FixStartResult{}, err
		}
		title := commitReviewThreadTitle(opts)
		starter := commitReviewStarter(opts)
		threadID, err := b.CreateWorkflowThread(channelID, title, starter)
		if err != nil {
			log.Printf("commit-review: create Discord thread failed project=%s: %v — web-native fallback", project, err)
			return b.startWebNativeUnit(project, cwd, prompt, opts.Actor, func(unitID string) error {
				return b.bindCommitReviewSession(unitID, project, goal, opts.Actor, "", true)
			})
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := b.bindCommitReviewSession(threadID, project, goal, opts.Actor, discordURL, true); err != nil {
			return FixStartResult{}, err
		}
		return b.startWebTask(threadID, project, cwd, prompt, opts.Actor, discordURL, true)
	}
	return b.startWebNativeUnit(project, cwd, prompt, opts.Actor, func(unitID string) error {
		return b.bindCommitReviewSession(unitID, project, goal, opts.Actor, "", true)
	})
}

func (b *Bot) bindCommitReviewSession(threadID, project, goal string, actor Actor, discordURL string, isNew bool) error {
	if b.sessions == nil {
		return fmt.Errorf("sessions store nil")
	}
	_, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if ent.Project == "" {
			ent.Project = project
		}
		if isNew || ent.Origin == "" {
			ent.Origin = SourceWeb
		}
		if isNew || ent.CreatedBy == "" {
			ent.CreatedBy = actor.ID
			ent.CreatedByName = actor.DisplayName
		}
		if discordURL != "" && ent.DiscordURL == "" {
			ent.DiscordURL = discordURL
		}
		if goal != "" && (isNew || ent.Goal == "") {
			ent.Goal = goal
		}
		if actor.ID != "" {
			ensureSessionOwner(ent, actor.ID, actor.String())
		}
	})
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	e := sessionstore.Entry{
		Project:       project,
		Origin:        SourceWeb,
		CreatedBy:     actor.ID,
		CreatedByName: actor.DisplayName,
		DiscordURL:    discordURL,
		Goal:          goal,
	}
	if actor.ID != "" {
		ensureSessionOwner(&e, actor.ID, actor.String())
	}
	return b.sessions.Set(threadID, e)
}

func commitReviewThreadTitle(opts CommitReviewOpts) string {
	short := strings.TrimSpace(opts.ShortSHA)
	if short == "" {
		sha := strings.TrimSpace(opts.SHA)
		if len(sha) >= 7 {
			short = sha[:7]
		} else {
			short = sha
		}
	}
	summary := "Review " + short
	if sub := strings.TrimSpace(opts.Subject); sub != "" {
		summary = summary + " " + sub
	}
	name := threadNameFromPrompt(summary, opts.Actor.DisplayName)
	if len(name) > 100 {
		name = name[:97] + "…"
	}
	return name
}

func commitReviewStarter(opts CommitReviewOpts) string {
	who := opts.Actor.DisplayName
	if who == "" {
		who = opts.Actor.ID
	}
	if who == "" {
		who = "web"
	}
	short := strings.TrimSpace(opts.ShortSHA)
	if short == "" {
		sha := strings.TrimSpace(opts.SHA)
		if len(sha) >= 7 {
			short = sha[:7]
		} else {
			short = sha
		}
	}
	line := fmt.Sprintf("**Grok Work** · Commit review `%s` · started by %s (web)", short, who)
	if owner, repo, sha := strings.TrimSpace(opts.Owner), strings.TrimSpace(opts.Repo), strings.TrimSpace(opts.SHA); owner != "" && repo != "" && sha != "" {
		line += fmt.Sprintf("\nhttps://github.com/%s/%s/commit/%s", owner, repo, sha)
	}
	return line
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
### How to review
1. Inspect this commit: ` + "`git show " + sha + "`" + ` (and surrounding context with read tools as needed).
2. Look for correctness bugs, security issues, missing tests for risky changes, broken contracts, data loss, concurrency hazards.
3. Do not bikeshed style/naming/formatting unless it causes a real defect.
4. Do not invent files or lines you did not see.

### Filing issues (agentic)
For each real finding, open a GitHub issue yourself with ` + "`gh`" + `:
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
- If the commit looks fine, open zero issues and say so clearly.
- Do not merge anything. Code fixes are optional: only implement a fix in this worktree if it is clearly warranted; otherwise filing issues is enough.
- In your final reply, list issue URLs (or state that none were filed) and a short overall assessment.
`)
	return b.String()
}
