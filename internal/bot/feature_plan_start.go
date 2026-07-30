package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// FeaturePlanOpts starts a new work unit from the web "Plan this feature" action.
// Always creates a new web-native session; never reuses one, never opens a Discord
// thread. The agent writes a GitHub tasklist onto the issue body.
type FeaturePlanOpts struct {
	Project string
	Owner   string
	Repo    string
	Number  int
	Title   string
	URL     string
	Body    string
	Actor   Actor
	// Model overrides the configured review model for this session. Empty takes
	// config's review model (then the task model). Requires builder-class caps.
	Model string
}

// StartFeaturePlan creates a workflow unit and enqueues an agentic feature-plan task.
// Deliberately does not bind the issue: a planning session is read-mostly, and
// binding would put it in the Fix reuse picker for a session that only ever
// rewrote the Breakdown section (same reasoning as StartPRReview's non-binding).
func (b *Bot) StartFeaturePlan(opts FeaturePlanOpts) (FixStartResult, error) {
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
		return FixStartResult{}, ErrInvalidIssue
	}

	// Same gate and resolution order as the other dispatch cards: authorize a named
	// model first, then resolve it (or the configured review default), so a forged
	// form value fails the request instead of silently downgrading.
	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	goal := featurePlanGoal(opts)
	return b.startWebNativeUnit(project, cwd, BuildFeaturePlanPrompt(opts), KindTask, opts.Actor, nil,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			return b.stampNewSessionCLI(unitID, cli)
		})
}

func featurePlanGoal(opts FeaturePlanOpts) string {
	sel := fmt.Sprintf("%s/%s#%d", strings.TrimSpace(opts.Owner), strings.TrimSpace(opts.Repo), opts.Number)
	goal := "Plan " + sel
	if title := strings.TrimSpace(opts.Title); title != "" {
		goal += " — " + title
	}
	return clampGoal(goal)
}

// BuildFeaturePlanPrompt is the web-started agentic feature-plan task body.
// Callers still prepend remoteWorkPromptPrefix at execute time.
// The single deliverable is a GitHub tasklist written onto the issue body.
func BuildFeaturePlanPrompt(opts FeaturePlanOpts) string {
	actorDisplay := strings.TrimSpace(opts.Actor.DisplayName)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	slug := owner + "/" + repo
	num := opts.Number
	issueURL := strings.TrimSpace(opts.URL)
	if issueURL == "" {
		issueURL = fmt.Sprintf("https://github.com/%s/issues/%d", slug, num)
	}
	body := truncateRunes(strings.TrimSpace(opts.Body), fixPromptBodyMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (Plan feature from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "Read feature issue %s#%d and the repository, then produce an implementation breakdown as a **GitHub tasklist** on that issue.\n", slug, num)
	b.WriteString("You own the issue edit end-to-end (agentic): fetch the current body, append (or replace a previous) Breakdown section, and write it back with `gh`.\n\n")

	b.WriteString("### Feature issue\n")
	fmt.Fprintf(&b, "- Repo: %s\n", slug)
	fmt.Fprintf(&b, "- Number: #%d\n", num)
	if t := strings.TrimSpace(opts.Title); t != "" {
		fmt.Fprintf(&b, "- Title: %s\n", t)
	}
	fmt.Fprintf(&b, "- URL: %s\n", issueURL)
	if body != "" {
		b.WriteString("\n### Issue body\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	numStr := fmt.Sprint(num)
	issueRef := numStr + " --repo " + slug
	b.WriteString(`
### How to write the breakdown
1. Skim the issue and the repo enough to know the real seams (packages, entry points, existing tests). Do not invent files you have not seen.
2. Fetch the current issue body:
   ` + "`gh issue view " + issueRef + " --json body -q .body`" + `
3. Append (or replace a previous one) a section **exactly** like:
` + "```" + `
## Breakdown
<!-- grokwork:tasklist -->
- [ ] first sub-task
- [ ] second sub-task
` + "```" + `
   - Keep each item **one line**, self-contained, implementable by one agent session.
   - Typical size: **3–8 items**. Prefer fewer, sharper items over a laundry list.
   - Never touch text outside the Breakdown section. If a prior Breakdown exists, replace that section only.
4. Write the body back with a temp file (survives backticks, ` + "`#`" + `, newlines that inline ` + "`--body`" + ` would mangle):
   ` + "`gh issue edit " + issueRef + " --body-file <file>`" + `

Rules:
- **Plan only.** Do not implement anything, do not open PRs, do not create issues, do not merge.
- In your final reply: summarize the items you wrote and link the issue.
`)
	return b.String()
}

// ChecklistItemOpts starts a new work unit for one tasklist line of a feature issue.
type ChecklistItemOpts struct {
	Project    string
	Owner      string
	Repo       string
	Number     int
	IssueTitle string
	IssueURL   string
	ItemText   string
	RawLine    string
	Actor      Actor
	// Model overrides the configured review model for this session. Empty takes
	// config's review model (then the task model). Requires builder-class caps.
	Model string
}

// StartChecklistItem creates a web-native KindTask unit scoped to one breakdown
// item, binds the parent issue as Refs, and best-effort annotates the checklist
// line with the new session URL.
func (b *Bot) StartChecklistItem(opts ChecklistItemOpts) (FixStartResult, error) {
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
		return FixStartResult{}, ErrInvalidIssue
	}
	itemText := strings.TrimSpace(opts.ItemText)
	if itemText == "" {
		return FixStartResult{}, fmt.Errorf("empty checklist item text")
	}

	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	tracked := sessionstore.TrackedIssue{
		Owner:   strings.TrimSpace(opts.Owner),
		Repo:    strings.TrimSpace(opts.Repo),
		Number:  opts.Number,
		Title:   strings.TrimSpace(opts.IssueTitle),
		URL:     strings.TrimSpace(opts.IssueURL),
		Keyword: sessionstore.IssueKeywordRefs,
	}
	tracked.FillFromURL()

	goal := clampGoal(itemText)
	prompt := BuildChecklistItemPrompt(opts)

	res, err := b.startWebNativeUnit(project, cwd, prompt, KindTask, opts.Actor, nil,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			if err := b.stampNewSessionCLI(unitID, cli); err != nil {
				return err
			}
			// Bind Refs (not Fixes): one sub-task rarely closes the whole feature.
			if b.sessions == nil {
				return fmt.Errorf("sessions store nil")
			}
			_, _, patchErr := b.sessions.Patch(unitID, func(ent *sessionstore.Entry) {
				ent.UpsertIssueForceKeyword(tracked)
			})
			return patchErr
		})
	if err != nil {
		return res, err
	}

	// Best-effort annotation: session must start even when the issue body cannot
	// be written. Skip entirely when there is no public base URL — no useful link.
	b.annotateChecklistItemSession(res.ThreadID, cwd, opts)
	return res, nil
}

// annotateChecklistItemSession appends a session link to the matching tasklist
// line. Failures log only (and audit when the edit was attempted).
func (b *Bot) annotateChecklistItemSession(unitID, cwd string, opts ChecklistItemOpts) {
	if b == nil {
		return
	}
	url := b.sessionWebURL(unitID)
	if url == "" {
		return
	}
	rawLine := lineContent(opts.RawLine)
	if rawLine == "" {
		return
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	num := opts.Number

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := b.ghRun()
	info, err := ghpr.ViewIssueWith(ctx, run, cwd, num, owner, repo)
	if err != nil {
		log.Printf("checklist annotate: view %s/%s#%d: %v", owner, repo, num, err)
		b.auditChecklistLink(opts.Actor, unitID, opts.Project, owner, repo, num, err)
		return
	}
	newBody, changed := AnnotateTasklistLine(info.Body, rawLine, url)
	if !changed {
		return
	}
	if err := ghpr.EditIssueBodyWith(ctx, run, cwd, owner, repo, num, newBody); err != nil {
		log.Printf("checklist annotate: edit %s/%s#%d: %v", owner, repo, num, err)
		b.auditChecklistLink(opts.Actor, unitID, opts.Project, owner, repo, num, err)
		return
	}
	b.auditChecklistLink(opts.Actor, unitID, opts.Project, owner, repo, num, nil)
}

func (b *Bot) auditChecklistLink(actor Actor, threadID, project, owner, repo string, number int, err error) {
	if b == nil || b.audit == nil {
		return
	}
	detail := map[string]any{
		"project":  project,
		"owner":    owner,
		"repo":     repo,
		"number":   number,
		"threadId": threadID,
		"source":   "web",
	}
	b.auditCmd(audit.ActionIssueChecklistLink, actor, threadID, project, err, detail)
}

// SetGHRunner installs an injectable ghpr.Runner (nil clears to the package
// default). Install at construction/test setup only — the field is read from
// pollers and dispatch goroutines without a lock, so a request-time write races.
func (b *Bot) SetGHRunner(run ghpr.Runner) {
	if b == nil {
		return
	}
	b.ghRunner = run
}

// ghRun returns the injectable Runner; nil → ghpr default.
func (b *Bot) ghRun() ghpr.Runner {
	if b != nil && b.ghRunner != nil {
		return b.ghRunner
	}
	return nil
}

// BuildChecklistItemPrompt is the web-started task body for one breakdown item.
func BuildChecklistItemPrompt(opts ChecklistItemOpts) string {
	actorDisplay := strings.TrimSpace(opts.Actor.DisplayName)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	slug := owner + "/" + repo
	num := opts.Number
	issueURL := strings.TrimSpace(opts.IssueURL)
	if issueURL == "" && owner != "" && repo != "" && num > 0 {
		issueURL = fmt.Sprintf("https://github.com/%s/issues/%d", slug, num)
	}
	itemText := strings.TrimSpace(opts.ItemText)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	fmt.Fprintf(&b, "This session implements **one sub-task** of feature issue %s#%d.\n", slug, num)
	if t := strings.TrimSpace(opts.IssueTitle); t != "" {
		fmt.Fprintf(&b, "Feature title: %s\n", t)
	}
	if issueURL != "" {
		fmt.Fprintf(&b, "Feature URL: %s\n", issueURL)
	}
	b.WriteString("\n### Your scope\n")
	fmt.Fprintf(&b, "Your scope is exactly this sub-task of the breakdown: %s\n", itemText)
	b.WriteString("Stay inside it; other items belong to other sessions.\n")
	fmt.Fprintf(&b, "\nThe feature issue is referenced as Refs %s#%d (do not claim Fixes unless this item truly closes the whole feature).\n", slug, num)
	return b.String()
}
