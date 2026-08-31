package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

const prAskDiffMaxRunes = 80_000

// PRAskOpts starts or continues a throwaway in-page Q&A on a pull request.
type PRAskOpts struct {
	Project string
	Actor   Actor

	Owner  string
	Repo   string
	Number int
	Title  string
	URL    string
	State  string
	HeadSHA      string
	HeadRef      string
	BaseRef      string
	Body         string
	Author       string
	Additions    int
	Deletions    int
	ChangedFiles int
	// Diff is a truncated unified patch the host already fetched (best-effort).
	// Investigate runs have no GH_TOKEN, so the prompt carries the change list.
	Diff string
	// Question is the reviewer's prompt. Required.
	Question string
	// Model overrides the configured review model on first create only.
	Model string
}

// StartPRAsk creates or reuses a SessionKindPRAsk unit for this viewer+PR and
// enqueues an investigate turn. Never binds PRs[], never opens Discord.
func (b *Bot) StartPRAsk(opts PRAskOpts) (FixStartResult, error) {
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
	question := strings.TrimSpace(opts.Question)
	if question == "" {
		return FixStartResult{}, ErrEmptyPrompt
	}

	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	askKey := sessionstore.FormatAskPRKey(opts.Owner, opts.Repo, opts.Number)
	if askKey == "" {
		return FixStartResult{}, ErrInvalidPR
	}
	prompt := BuildPRAskPrompt(opts)

	if existing := b.FindPRAsk(project, askKey, opts.Actor.ID); existing != "" {
		return b.startWebTask(existing, project, cwd, prompt, KindStartInvestigate, opts.Actor, "", nil, false)
	}

	goal := prAskGoal(opts)
	return b.startWebNativeUnit(project, cwd, prompt, KindStartInvestigate, opts.Actor, nil,
		func(unitID string) error {
			if err := b.bindWebStartedSession(unitID, project, goal, opts.Actor, "", true); err != nil {
				return err
			}
			if err := b.bindPRAskUnit(unitID, project, askKey, opts.Actor); err != nil {
				return err
			}
			return b.stampNewSessionCLI(unitID, cli)
		})
}

// userFacingPrompt is what history and the live user bubble should show.
// PR-ask turns wrap the reviewer's question in a stuffed contract; replay the
// question only so the PR page does not render an 80k diff as "You".
func (b *Bot) userFacingPrompt(threadID, prompt string) string {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return prompt
	}
	if e, ok := b.sessions.Get(threadID); ok && e.IsPRAsk() {
		return prAskQuestionFromPrompt(prompt)
	}
	return prompt
}

// FindPRAsk returns the throwaway ask unit for this viewer on this PR, if any.
func (b *Bot) FindPRAsk(project, askKey, actorID string) string {
	if b == nil || b.sessions == nil {
		return ""
	}
	project = strings.TrimSpace(project)
	askKey = strings.ToLower(strings.TrimSpace(askKey))
	actorID = strings.TrimSpace(actorID)
	if project == "" || askKey == "" {
		return ""
	}
	for _, listed := range b.sessions.List() {
		if !listed.IsPRAsk() {
			continue
		}
		if !strings.EqualFold(listed.Project, project) {
			continue
		}
		if strings.ToLower(strings.TrimSpace(listed.AskPRKey)) != askKey {
			continue
		}
		if strings.TrimSpace(listed.OwnerID) != actorID {
			continue
		}
		return listed.ThreadID
	}
	return ""
}

func (b *Bot) bindPRAskUnit(unitID, project, askKey string, actor Actor) error {
	if b.sessions == nil {
		return fmt.Errorf("sessions store nil")
	}
	_, ok, err := b.sessions.Patch(unitID, func(ent *sessionstore.Entry) {
		if ent.Project == "" {
			ent.Project = project
		}
		ent.SessionKind = sessionstore.SessionKindPRAsk
		ent.AskPRKey = askKey
		ent.WorktreeBranch = ""
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
		Project:     project,
		Origin:      SourceWeb,
		SessionKind: sessionstore.SessionKindPRAsk,
		AskPRKey:    askKey,
	}
	if actor.ID != "" {
		ensureSessionOwner(&e, actor.ID, actor.String())
	}
	return b.sessions.Set(unitID, e)
}

func prAskGoal(opts PRAskOpts) string {
	sel := fmt.Sprintf("%s/%s#%d", strings.TrimSpace(opts.Owner), strings.TrimSpace(opts.Repo), opts.Number)
	goal := "Ask " + sel
	if title := strings.TrimSpace(opts.Title); title != "" {
		goal += ": " + title
	}
	return clampGoal(goal)
}

// BuildPRAskPrompt is the in-page PR Q&A task body.
// Callers still prepend investigatePromptPrefix at execute time.
// The deliverable is the reply on the PR page — never a GitHub comment or commit.
func BuildPRAskPrompt(opts PRAskOpts) string {
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
	question := strings.TrimSpace(opts.Question)
	body := truncateRunes(strings.TrimSpace(opts.Body), fixPromptBodyMaxRunes)
	diff := truncateRunes(strings.TrimSpace(opts.Diff), prAskDiffMaxRunes)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (Ask about this PR from web by %s)\n", actorDisplay)
	b.WriteString("Answer the reviewer's question about this pull request.\n")
	b.WriteString("The working tree (when isolated) is checked out at the PR head.\n")
	b.WriteString("Cite `file:line` when you refer to code. If you did not see it, say so.\n\n")

	b.WriteString("### Question\n")
	b.WriteString(question)
	b.WriteString("\n\n")

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
	if diff != "" {
		b.WriteString("\n### Diff (host-fetched, may be truncated)\n```diff\n")
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
	}

	b.WriteString(`
Rules:
- **Read-only Q&A.** Do not edit files, do not commit, do not push, do not open or update a pull request, and never merge.
- Do not post a GitHub comment, review, or issue. Your reply on this page is the only deliverable.
- Do not run ` + "`gh pr comment`" + `, ` + "`gh pr review`" + `, ` + "`gh issue create`" + `, or ` + "`gh pr create`" + `.
- Prefer the files and diff above plus the checkout at PR head. Do not invent files or lines you have not seen.
- Answer the question. Do not produce a full unsolicited review unless asked.
`)
	return b.String()
}

// prAskQuestionFromPrompt pulls the reviewer's question out of BuildPRAskPrompt
// so history and the live user bubble do not replay the stuffed diff / contract.
func prAskQuestionFromPrompt(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "### Question\n")
	if !ok {
		return strings.TrimSpace(prompt)
	}
	q, _, _ := strings.Cut(rest, "\n\n### ")
	q = strings.TrimSpace(q)
	if q == "" {
		return strings.TrimSpace(prompt)
	}
	return q
}
