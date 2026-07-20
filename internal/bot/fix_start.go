package bot

import (
	"errors"
	"fmt"
	"strings"

	"github.com/acoshift/grok-discord/internal/sessionstore"
)

// Sentinel errors for web Fix-with-Grok mapping to HTTP status.
var (
	ErrDiscordNotReady = errors.New("discord gateway not ready")
	ErrPickerRequired  = errors.New("multiple sessions bind this issue; pick one")
	ErrLinearDisabled  = errors.New("linear is not enabled for this project")
	ErrProjectRequired = errors.New("project required")
	ErrInvalidIssue    = errors.New("invalid issue")
)

// FixKind selects GitHub vs Linear fix start.
type FixKind string

const (
	FixKindGitHub FixKind = "github"
	FixKindLinear FixKind = "linear"
)

// FixStartOpts starts or reuses a work unit from the web Fix-with-Grok action.
type FixStartOpts struct {
	Kind     FixKind
	Project  string
	Actor    Actor
	ForceNew bool
	// ThreadID forces reuse of a specific unit (picker selection). Empty → discover.
	ThreadID string

	// GitHub
	Owner  string
	Repo   string
	Number int
	// Linear
	Identifier string
	LinearID   string

	// Shared presentation fields (title/body for prompt + bind metadata).
	Title string
	URL   string
	Body  string // GitHub body or Linear description
	State string
}

// FixStartStatus is the outcome of StartFix.
type FixStartStatus string

const (
	FixStatusStarted FixStartStatus = "started"
	FixStatusQueued  FixStartStatus = "queued"
	FixStatusPicker  FixStartStatus = "picker"
	// FixStatusCreated is not used separately — create also yields started/queued.
)

// FixStartResult is returned from StartFix.
type FixStartResult struct {
	Status        FixStartStatus
	ThreadID      string
	QueuePos      int
	Hits          []IssueSessionHit // set when Status == picker
	DiscordOffline bool             // reuse path with Discord down
	DiscordURL    string
	Created       bool // true when a new Discord thread was opened
}

// StartFix discovers or creates a work unit, binds the issue with Fixes, and StartTasks.
// Reuse never calls CreateWorkflowThread. Create requires PreferDiscordChannel + DiscordReady.
func (b *Bot) StartFix(opts FixStartOpts) (FixStartResult, error) {
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

	switch opts.Kind {
	case FixKindGitHub:
		if opts.Number <= 0 || strings.TrimSpace(opts.Owner) == "" || strings.TrimSpace(opts.Repo) == "" {
			return FixStartResult{}, ErrInvalidIssue
		}
	case FixKindLinear:
		if !b.cfg.ProjectLinearEnabled(project) {
			return FixStartResult{}, ErrLinearDisabled
		}
		if sessionstore.NormalizeLinearIdentifier(opts.Identifier) == "" {
			return FixStartResult{}, ErrInvalidIssue
		}
	default:
		return FixStartResult{}, fmt.Errorf("unknown fix kind %q", opts.Kind)
	}

	tracked := fixTrackedIssue(opts)
	prompt := fixPromptFor(opts)

	// Explicit picker selection → reuse only.
	if tid := strings.TrimSpace(opts.ThreadID); tid != "" && !opts.ForceNew {
		return b.startFixReuse(tid, project, cwd, tracked, prompt, opts.Actor)
	}

	if !opts.ForceNew {
		var hits []IssueSessionHit
		switch opts.Kind {
		case FixKindGitHub:
			hits = b.FindByIssue(project, opts.Owner, opts.Repo, opts.Number, false)
		case FixKindLinear:
			hits = b.FindByLinearIssue(project, opts.Identifier, false)
		}
		switch len(hits) {
		case 0:
			// fall through to create
		case 1:
			return b.startFixReuse(hits[0].ThreadID, project, cwd, tracked, prompt, opts.Actor)
		default:
			return FixStartResult{Status: FixStatusPicker, Hits: hits}, ErrPickerRequired
		}
	}

	return b.startFixCreate(project, cwd, tracked, prompt, opts)
}

func fixTrackedIssue(opts FixStartOpts) sessionstore.TrackedIssue {
	switch opts.Kind {
	case FixKindLinear:
		return sessionstore.TrackedIssue{
			Provider:   sessionstore.ProviderLinear,
			Identifier: sessionstore.NormalizeLinearIdentifier(opts.Identifier),
			LinearID:   strings.TrimSpace(opts.LinearID),
			Title:      strings.TrimSpace(opts.Title),
			URL:        strings.TrimSpace(opts.URL),
			State:      strings.TrimSpace(opts.State),
			Keyword:    sessionstore.IssueKeywordFixes,
		}
	default:
		iss := sessionstore.TrackedIssue{
			Owner:   strings.TrimSpace(opts.Owner),
			Repo:    strings.TrimSpace(opts.Repo),
			Number:  opts.Number,
			Title:   strings.TrimSpace(opts.Title),
			URL:     strings.TrimSpace(opts.URL),
			Keyword: sessionstore.IssueKeywordFixes,
		}
		iss.FillFromURL()
		return iss
	}
}

func fixPromptFor(opts FixStartOpts) string {
	switch opts.Kind {
	case FixKindLinear:
		return BuildLinearFixPrompt(opts.Actor.DisplayName, opts.Identifier, opts.Title, opts.URL, opts.State, opts.Body)
	default:
		return BuildGitHubFixPrompt(opts.Actor.DisplayName, opts.Owner, opts.Repo, opts.Number, opts.Title, opts.URL, opts.Body)
	}
}

func (b *Bot) startFixReuse(threadID, project, cwd string, tracked sessionstore.TrackedIssue, prompt string, actor Actor) (FixStartResult, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return FixStartResult{}, fmt.Errorf("empty thread id")
	}
	// Bind Fixes onto existing session (create entry shell if missing).
	if err := b.bindFixIssue(threadID, project, tracked, actor, "", false); err != nil {
		return FixStartResult{}, err
	}
	discordURL := ""
	if e, ok := b.sessions.Get(threadID); ok {
		discordURL = e.DiscordURL
	}
	if discordURL == "" {
		discordURL = DiscordThreadURL(b.cfg.DiscordGuildIDValue(), threadID)
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

func (b *Bot) startFixCreate(project, cwd string, tracked sessionstore.TrackedIssue, prompt string, opts FixStartOpts) (FixStartResult, error) {
	channelID, err := b.cfg.PreferDiscordChannel(project)
	if err != nil {
		return FixStartResult{}, err
	}
	if !b.DiscordReady() && b.threadAPI == nil {
		// threadAPI inject allows tests without a live gateway.
		return FixStartResult{}, ErrDiscordNotReady
	}

	title := fixThreadTitle(tracked, opts)
	starter := fixStarterContent(tracked, opts.Actor)
	threadID, err := b.CreateWorkflowThread(channelID, title, starter)
	if err != nil {
		return FixStartResult{}, fmt.Errorf("create workflow thread: %w", err)
	}
	discordURL := DiscordThreadURL(b.cfg.DiscordGuildIDValue(), threadID)
	if err := b.bindFixIssue(threadID, project, tracked, opts.Actor, discordURL, true); err != nil {
		return FixStartResult{}, err
	}
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID:      threadID,
		Proj:          projectRef{Name: project, Cwd: cwd},
		Prompt:        prompt,
		Actor:         opts.Actor,
		Source:        SourceWeb,
		Origin:        SourceWeb,
		CreatedBy:     opts.Actor.ID,
		CreatedByName: opts.Actor.DisplayName,
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
		Status:     st,
		ThreadID:   threadID,
		QueuePos:   pos,
		DiscordURL: discordURL,
		Created:    true,
	}, nil
}

func (b *Bot) bindFixIssue(threadID, project string, tracked sessionstore.TrackedIssue, actor Actor, discordURL string, isNew bool) error {
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
		ent.UpsertIssueForceKeyword(tracked)
	})
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// Create shell entry.
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
	e.UpsertIssueForceKeyword(tracked)
	return b.sessions.Set(threadID, e)
}

func fixThreadTitle(tracked sessionstore.TrackedIssue, opts FixStartOpts) string {
	summary := strings.TrimSpace(opts.Title)
	if summary == "" {
		summary = "Fix " + tracked.DisplayRef()
	}
	name := threadNameFromPrompt(summary, opts.Actor.DisplayName)
	pref := strings.TrimSpace(sessionstore.IssueTitlePrefix([]sessionstore.TrackedIssue{tracked}))
	if pref != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(strings.TrimSpace(pref))) {
		name = strings.TrimSpace(pref + " " + name)
	}
	if len(name) > 100 {
		name = name[:97] + "…"
	}
	return name
}

func fixStarterContent(tracked sessionstore.TrackedIssue, actor Actor) string {
	who := actor.DisplayName
	if who == "" {
		who = actor.ID
	}
	if who == "" {
		who = "web"
	}
	ref := tracked.DisplayRef()
	line := fmt.Sprintf("**Grok Work** · Fix %s · started by %s (web)", ref, who)
	if u := strings.TrimSpace(tracked.URL); u != "" {
		line += "\n" + u
	}
	return line
}
