package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Additional sentinels for Address CI / Continue / Address review.
var (
	ErrInvalidPR        = fmt.Errorf("invalid pull request")
	ErrEmptyPrompt      = fmt.Errorf("empty prompt")
	ErrUnknownThread    = fmt.Errorf("unknown session/thread")
	ErrNoReviewComments = fmt.Errorf("no unresolved review comments and no PR comments to work through")
)

// AddressCIOpts starts a single-PR CI fix from the web.
type AddressCIOpts struct {
	Project  string
	Actor    Actor
	ForceNew bool
	ThreadID string // picker selection

	Owner  string
	Repo   string
	Number int
	Title  string
	URL    string
	State  string
	// Optional CI context (web may pre-fetch; empty → generic single-PR prompt).
	HeadSHA    string
	HeadRef    string
	Checks     string
	Failed     []ghpr.Check
	LogSnippet string
	// Model applies only when this dispatch creates a session; a reused session
	// keeps the agent it was stamped with. Empty takes the configured review
	// model. Requires builder-class caps.
	Model string
}

// StartAddressCI reuses a unit by PR or creates one, binds the PR, and StartTasks.
// Prompt is always single-PR (unlike Discord /fix-ci multi-PR).
func (b *Bot) StartAddressCI(opts AddressCIOpts) (FixStartResult, error) {
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

	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	tracked := addressTrackedPR(opts)
	prompt := BuildAddressCIPrompt(opts)

	if tid := strings.TrimSpace(opts.ThreadID); tid != "" && !opts.ForceNew {
		return b.startPRReuse(tid, project, cwd, tracked, prompt, opts.Actor)
	}
	if !opts.ForceNew {
		hits := b.FindByPR(project, opts.Owner, opts.Repo, opts.Number, false)
		switch len(hits) {
		case 0:
			// create
		case 1:
			return b.startPRReuse(hits[0].ThreadID, project, cwd, tracked, prompt, opts.Actor)
		default:
			return FixStartResult{Status: FixStatusPicker, Hits: hits}, ErrPickerRequired
		}
	}
	return b.startPRCreate(project, cwd, tracked, prompt, opts.Actor, cli, fmt.Sprintf("CI %s/%s#%d", opts.Owner, opts.Repo, opts.Number))
}

// resolveDispatchCLI is the shared model gate for the PR dispatch cards: authorize
// a named model, then resolve it (or the configured review default). Called before
// the reuse branches so an unauthorized or bogus model fails the request outright
// rather than only on the create path, where the caller could not tell which of the
// two it got.
func (b *Bot) resolveDispatchCLI(project string, actor Actor, model string) (config.AgentCLI, error) {
	if strings.TrimSpace(model) != "" {
		if err := b.requireCanSelectModel(project, actor.ID); err != nil {
			return config.AgentCLI{}, err
		}
	}
	return b.cfg.ReviewAgentCLI(model)
}

// ContinueOpts queues a freeform follow-up on an existing thread only.
type ContinueOpts struct {
	ThreadID        string
	Project         string // optional; taken from session when empty
	Prompt          string
	Actor           Actor
	AttachmentPaths []string // staged web images; ownership transfers to StartTask
}

// StartContinue runs StartTask on an existing work unit (never creates a thread).
func (b *Bot) StartContinue(opts ContinueOpts) (FixStartResult, error) {
	if b == nil {
		return FixStartResult{}, fmt.Errorf("bot is nil")
	}
	threadID := strings.TrimSpace(opts.ThreadID)
	if threadID == "" {
		return FixStartResult{}, ErrUnknownThread
	}
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return FixStartResult{}, ErrEmptyPrompt
	}
	// Soft prefix so model keeps contract on follow-ups from web.
	if !strings.Contains(strings.ToLower(prompt), "do not merge") {
		prompt = prompt + "\n\n(When you open or update a PR: do not merge.)"
	}

	e, ok := b.sessions.Get(threadID)
	if !ok {
		return FixStartResult{}, ErrUnknownThread
	}
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		project = e.Project
	}
	if project == "" {
		return FixStartResult{}, ErrProjectRequired
	}
	cwd, ok := b.cfg.ProjectPath(project)
	if !ok || strings.TrimSpace(cwd) == "" {
		return FixStartResult{}, fmt.Errorf("unknown project %q", project)
	}
	if e.MainCwd != "" {
		cwd = e.MainCwd
	}

	discordURL := e.DiscordURL
	if discordURL == "" {
		discordURL = DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
	}
	offline := !b.DiscordReady()
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID:        threadID,
		Proj:            projectRef{Name: project, Cwd: cwd},
		Prompt:          prompt,
		Actor:           opts.Actor,
		Source:          SourceWeb,
		Origin:          SourceWeb,
		CreatedBy:       opts.Actor.ID,
		CreatedByName:   opts.Actor.DisplayName,
		DiscordURL:      discordURL,
		DG:              b.Discord(),
		AttachmentPaths: opts.AttachmentPaths,
	})
	if err != nil {
		return FixStartResult{}, err
	}
	st := FixStatusStarted
	if pos > 0 {
		st = FixStatusQueued
	}
	return FixStartResult{
		Status:         st,
		ThreadID:       threadID,
		QueuePos:       pos,
		DiscordOffline: offline,
		DiscordURL:     discordURL,
		Created:        false,
	}, nil
}

// AddressReviewOpts starts a run to address unresolved PR review comments.
type AddressReviewOpts struct {
	Project  string
	Actor    Actor
	ForceNew bool
	ThreadID string

	Owner  string
	Repo   string
	Number int
	Title  string
	URL    string
	// Comments are unresolved inline review threads. Caller fails closed if the
	// list call failed.
	Comments []ghpr.ReviewComment
	// Conversation is the PR's top-level comment thread. Separate from Comments
	// because it is a different kind of instruction: an inline thread points at
	// a line and is unresolved by definition, while a conversation comment may
	// be chatter, may already be handled, and — importantly — is where an agent
	// review lands, since StartPRReview posts its findings with `gh pr comment`.
	// Without this the review loop could not close: the reviewer wrote its
	// findings somewhere this dispatch never read.
	Conversation []ghpr.IssueComment
	// Model applies only when this dispatch creates a session — see AddressCIOpts.
	Model string
}

// StartAddressReview reuses/creates a unit, binds PR, and runs with review prompt.
func (b *Bot) StartAddressReview(opts AddressReviewOpts) (FixStartResult, error) {
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
	// Either source alone is enough work to do: a reviewer who wrote "please
	// rework the retry logic" as a plain comment left no unresolved thread.
	if len(opts.Comments) == 0 && len(opts.Conversation) == 0 {
		return FixStartResult{}, ErrNoReviewComments
	}
	cli, err := b.resolveDispatchCLI(project, opts.Actor, opts.Model)
	if err != nil {
		return FixStartResult{}, err
	}

	tracked := sessionstore.TrackedPR{
		Owner:  strings.TrimSpace(opts.Owner),
		Repo:   strings.TrimSpace(opts.Repo),
		Number: opts.Number,
		Title:  strings.TrimSpace(opts.Title),
		URL:    strings.TrimSpace(opts.URL),
		State:  "OPEN",
	}
	tracked.FillOwnerRepoFromURL()
	prompt := BuildAddressReviewPrompt(opts)

	if tid := strings.TrimSpace(opts.ThreadID); tid != "" && !opts.ForceNew {
		return b.startPRReuse(tid, project, cwd, tracked, prompt, opts.Actor)
	}
	if !opts.ForceNew {
		hits := b.FindByPR(project, opts.Owner, opts.Repo, opts.Number, false)
		switch len(hits) {
		case 0:
		case 1:
			return b.startPRReuse(hits[0].ThreadID, project, cwd, tracked, prompt, opts.Actor)
		default:
			return FixStartResult{Status: FixStatusPicker, Hits: hits}, ErrPickerRequired
		}
	}
	return b.startPRCreate(project, cwd, tracked, prompt, opts.Actor, cli, fmt.Sprintf("Review %s/%s#%d", opts.Owner, opts.Repo, opts.Number))
}

func addressTrackedPR(opts AddressCIOpts) sessionstore.TrackedPR {
	pr := sessionstore.TrackedPR{
		Owner:   strings.TrimSpace(opts.Owner),
		Repo:    strings.TrimSpace(opts.Repo),
		Number:  opts.Number,
		Title:   strings.TrimSpace(opts.Title),
		URL:     strings.TrimSpace(opts.URL),
		State:   strings.TrimSpace(opts.State),
		HeadSHA: strings.TrimSpace(opts.HeadSHA),
		HeadRef: strings.TrimSpace(opts.HeadRef),
		Checks:  strings.TrimSpace(opts.Checks),
	}
	if pr.State == "" {
		pr.State = "OPEN"
	}
	pr.FillOwnerRepoFromURL()
	return pr
}

func (b *Bot) startPRReuse(threadID, project, cwd string, tracked sessionstore.TrackedPR, prompt string, actor Actor) (FixStartResult, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return FixStartResult{}, ErrUnknownThread
	}
	if err := b.bindTrackedPR(threadID, project, tracked, actor, "", false); err != nil {
		return FixStartResult{}, err
	}
	discordURL := ""
	if e, ok := b.sessions.Get(threadID); ok {
		discordURL = e.DiscordURL
		if e.MainCwd != "" {
			cwd = e.MainCwd
		}
	}
	if discordURL == "" {
		discordURL = DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
	}
	offline := !b.DiscordReady()
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID:      threadID,
		Proj:          projectRef{Name: project, Cwd: cwd},
		Prompt:        prompt,
		Actor:         actor,
		Source:        SourceWeb,
		Origin:        SourceWeb,
		CreatedBy:     actor.ID,
		CreatedByName: actor.DisplayName,
		DiscordURL:    discordURL,
		DG:            b.Discord(),
	})
	if err != nil {
		return FixStartResult{}, err
	}
	st := FixStatusStarted
	if pos > 0 {
		st = FixStatusQueued
	}
	return FixStartResult{
		Status:         st,
		ThreadID:       threadID,
		QueuePos:       pos,
		DiscordOffline: offline,
		DiscordURL:     discordURL,
		Created:        false,
	}, nil
}

// startPRCreate always allocates a web-native unit: a dispatch from the PR page
// streams back onto the session page it redirects to, and the PR itself is already
// the place the team watches this work. titleHint is kept for the session goal.
func (b *Bot) startPRCreate(project, cwd string, tracked sessionstore.TrackedPR, prompt string, actor Actor, cli config.AgentCLI, titleHint string) (FixStartResult, error) {
	goal := strings.TrimSpace(titleHint)
	if goal == "" {
		goal = tracked.Selector()
	}
	return b.startWebNativeUnit(project, cwd, prompt, KindTask, actor, func(unitID string) error {
		if err := b.bindTrackedPR(unitID, project, tracked, actor, "", true); err != nil {
			return err
		}
		if err := b.stampNewSessionCLI(unitID, cli); err != nil {
			return err
		}
		return b.setSessionGoalIfEmpty(unitID, goal)
	})
}

// setSessionGoalIfEmpty labels a freshly created unit. bindTrackedPR carries the PR
// but no goal, and without a Discord thread name there is nothing else for the
// sessions list to show.
func (b *Bot) setSessionGoalIfEmpty(threadID, goal string) error {
	goal = clampGoal(strings.TrimSpace(goal))
	if b.sessions == nil || goal == "" {
		return nil
	}
	_, _, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		if strings.TrimSpace(e.Goal) == "" {
			e.Goal = goal
		}
	})
	return err
}

func (b *Bot) bindTrackedPR(threadID, project string, tracked sessionstore.TrackedPR, actor Actor, discordURL string, isNew bool) error {
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
	e := sessionstore.Entry{
		Project:       project,
		Origin:        SourceWeb,
		CreatedBy:     actor.ID,
		CreatedByName: actor.DisplayName,
		DiscordURL:    discordURL,
	}
	if actor.ID != "" {
		ensureSessionOwner(&e, actor.ID, actor.String())
	}
	e.UpsertPR(tracked)
	return b.sessions.Set(threadID, e)
}

// BuildAddressCIPrompt is the single-PR CI fix task body for web (uses buildFixCIPrompt core).
func BuildAddressCIPrompt(opts AddressCIOpts) string {
	info := ghpr.Info{
		Number:  opts.Number,
		URL:     strings.TrimSpace(opts.URL),
		Title:   strings.TrimSpace(opts.Title),
		State:   strings.TrimSpace(opts.State),
		HeadSHA: strings.TrimSpace(opts.HeadSHA),
		HeadRef: strings.TrimSpace(opts.HeadRef),
		Checks:  strings.TrimSpace(opts.Checks),
		Owner:   strings.TrimSpace(opts.Owner),
		Repo:    strings.TrimSpace(opts.Repo),
	}
	if info.URL == "" && info.Owner != "" && info.Repo != "" && info.Number > 0 {
		info.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", info.Owner, info.Repo, info.Number)
	}
	branch := info.HeadRef
	core := buildFixCIPrompt(info, branch, opts.Failed, opts.LogSnippet)
	actor := strings.TrimSpace(opts.Actor.DisplayName)
	if actor == "" {
		actor = "web user"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Task (Address CI from web by %s)\n", actor)
	b.WriteString("Scope: this pull request only — do not touch other PRs on multi-PR threads unless required for this fix.\n\n")
	b.WriteString(core)
	b.WriteString("\nDo not merge the PR.\n")
	return b.String()
}

// BuildAddressReviewPrompt asks Grok to address unresolved review comments on one PR.
func BuildAddressReviewPrompt(opts AddressReviewOpts) string {
	actor := strings.TrimSpace(opts.Actor.DisplayName)
	if actor == "" {
		actor = "web user"
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	url := strings.TrimSpace(opts.URL)
	if url == "" && owner != "" && repo != "" && opts.Number > 0 {
		url = fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, opts.Number)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Task (Address review from web by %s)\n", actor)
	fmt.Fprintf(&b, "Address unresolved review comments on pull request %s/%s#%d", owner, repo, opts.Number)
	if t := strings.TrimSpace(opts.Title); t != "" {
		fmt.Fprintf(&b, ": %s", t)
	}
	b.WriteString(".\n")
	if url != "" {
		fmt.Fprintf(&b, "PR URL: %s\n", url)
	}
	if len(opts.Comments) == 0 {
		b.WriteString("\nNo unresolved inline review threads.\n")
	}
	if len(opts.Comments) > 0 {
		b.WriteString("\nUnresolved review comments:\n")
	}
	for i, c := range opts.Comments {
		fmt.Fprintf(&b, "\n### Comment %d", i+1)
		if c.Path != "" {
			fmt.Fprintf(&b, " · `%s`", c.Path)
			if c.Line > 0 {
				fmt.Fprintf(&b, ":%d", c.Line)
			}
		}
		b.WriteString("\n")
		if c.Author != "" {
			fmt.Fprintf(&b, "Author: %s\n", c.Author)
		}
		if c.URL != "" {
			fmt.Fprintf(&b, "URL: %s\n", c.URL)
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			body = "(empty)"
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	writeAddressConversation(&b, opts.Conversation)

	b.WriteString("\nTasks:\n")
	b.WriteString("1. Address each unresolved review comment with a minimal, correct change.\n")
	if len(opts.Conversation) > 0 {
		b.WriteString("2. Read the PR conversation and act on anything it asks for that is still outstanding. Some of it is discussion, and some may already be done — check the current code before changing anything, and say what you skipped and why.\n")
		b.WriteString("3. Run relevant tests when practical.\n")
		b.WriteString("4. Commit, push to the PR branch, and update the existing PR (do not open a duplicate).\n")
		b.WriteString("5. Summarize what you changed for each item you acted on.\n")
	} else {
		b.WriteString("2. Run relevant tests when practical.\n")
		b.WriteString("3. Commit, push to the PR branch, and update the existing PR (do not open a duplicate).\n")
		b.WriteString("4. Summarize what you changed for each comment.\n")
	}
	b.WriteString("Do not merge the PR.\n")
	return b.String()
}

// Prompt budget for the PR conversation. Unlike inline threads, which are
// unresolved by definition and therefore all actionable, a conversation is
// mostly history — the newest few carry the outstanding asks, and an old PR's
// full thread would crowd out the diff the agent actually needs to read.
const (
	maxPromptConversation     = 15
	maxPromptConversationBody = 4000
)

func writeAddressConversation(b *strings.Builder, comments []ghpr.IssueComment) {
	if len(comments) == 0 {
		return
	}
	omitted := 0
	if len(comments) > maxPromptConversation {
		omitted = len(comments) - maxPromptConversation
		comments = comments[len(comments)-maxPromptConversation:]
	}
	b.WriteString("\nPR conversation (top-level comments, oldest first")
	if omitted > 0 {
		// No count: the caller may already have capped its fetch, so any number
		// here would understate the drop. The PR URL above is the full thread.
		b.WriteString("; older ones omitted — read the PR itself if you need them")
	}
	b.WriteString("):\n")
	for i, c := range comments {
		fmt.Fprintf(b, "\n### PR comment %d", i+1)
		if a := strings.TrimSpace(c.Author); a != "" {
			fmt.Fprintf(b, " · %s", a)
		}
		if !c.CreatedAt.IsZero() {
			fmt.Fprintf(b, " · %s", c.CreatedAt.UTC().Format("2006-01-02 15:04"))
		}
		b.WriteString("\n")
		if u := strings.TrimSpace(c.URL); u != "" {
			fmt.Fprintf(b, "URL: %s\n", u)
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			body = "(empty)"
		} else if len(body) > maxPromptConversationBody {
			// Byte cut, so drop the partial rune it may end on: this prompt is
			// handed to a CLI (claude takes it on stdin as JSON), and invalid
			// UTF-8 breaks the encoding rather than just looking wrong.
			body = strings.ToValidUTF8(body[:maxPromptConversationBody], "") + "\n…(truncated)"
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
}
