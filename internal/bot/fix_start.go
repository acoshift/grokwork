package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Sentinel errors for web Fix-with-Grok mapping to HTTP status.
var (
	ErrDiscordNotReady = errors.New("discord gateway not ready")
	ErrPickerRequired  = errors.New("multiple sessions bind this issue; pick one")
	ErrLinearDisabled  = errors.New("linear is not enabled for this project")
	ErrClickUpDisabled = errors.New("clickup is not enabled for this project")
	ErrProjectRequired = errors.New("project required")
	ErrInvalidIssue    = errors.New("invalid issue")
	// ErrCannotStartFix is returned when the actor lacks builder-class ship caps
	// (startSessions + githubWrites) for an explicit fix / Fix-with-Grok start.
	ErrCannotStartFix = errors.New("you're not allowed to start fix tasks on this project (need startSessions and githubWrites)")
	// ErrCannotStartPlan is the same gate as fix: filing a GitHub plan issue is a write.
	ErrCannotStartPlan = errors.New("you're not allowed to start plan tasks on this project (need startSessions and githubWrites)")
	// ErrPlanModeConflict is /start plan (or mode=plan) on a unit already stamped
	// with another Mode. ensureSessionMode is first-writer-wins, so allowing the
	// run would plan once then let the next KindTask follow-up ship.
	ErrPlanModeConflict = errors.New("this session is already in another mode — start a new session to plan")
	// ErrCannotSelectModel is returned when the actor names a model without
	// builder-class caps. Choosing the model chooses the spend, so it sits behind
	// the same gate as shipping rather than plain startSessions.
	ErrCannotSelectModel = errors.New("you're not allowed to pick a model on this project (need startSessions and githubWrites)")
)

// FixKind selects GitHub vs Linear fix start.
type FixKind string

const (
	FixKindGitHub  FixKind = "github"
	FixKindLinear  FixKind = "linear"
	FixKindClickUp FixKind = "clickup"
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
	// ClickUp
	ClickUpID string
	CustomID  string

	// Shared presentation fields (title/body for prompt + bind metadata).
	Title string
	URL   string
	Body  string // GitHub body or Linear description
	State string
	// Model applies only when this dispatch creates a session; a reused session
	// keeps the agent it was stamped with. Empty takes the configured task model
	// at run start. Requires builder-class caps when non-empty.
	Model string
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
	Status         FixStartStatus
	ThreadID       string
	QueuePos       int
	Hits           []IssueSessionHit // set when Status == picker
	DiscordOffline bool              // reuse path with Discord down
	DiscordURL     string
	Created        bool // true when a new Discord thread was opened
}

// StartFix discovers or creates a work unit, binds the issue with Fixes, and StartTasks.
// Reuse never calls CreateWorkflowThread. Create prefers Discord when gateway/threadAPI is
// available; otherwise allocates a web-native unit (w_*) on grok/web/ without Discord.
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
	if err := b.requireCanStartFix(project, opts.Actor.ID); err != nil {
		return FixStartResult{}, err
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
	case FixKindClickUp:
		if !b.cfg.ProjectClickUpEnabled(project) {
			return FixStartResult{}, ErrClickUpDisabled
		}
		if strings.TrimSpace(opts.ClickUpID) == "" && sessionstore.NormalizeClickUpCustomID(opts.CustomID) == "" && strings.TrimSpace(opts.Identifier) == "" {
			return FixStartResult{}, ErrInvalidIssue
		}
	default:
		return FixStartResult{}, fmt.Errorf("unknown fix kind %q", opts.Kind)
	}

	// Authorize a named model before reuse/create so a forged form value fails the
	// request outright rather than only on the create path. Empty is the configured
	// default and needs no extra gate (same stance as freeform StartWebTask).
	model := strings.TrimSpace(opts.Model)
	var cli config.AgentCLI
	if model != "" {
		if err := b.requireCanSelectModel(project, opts.Actor.ID); err != nil {
			return FixStartResult{}, err
		}
		var err error
		if cli, err = b.cfg.RequestedAgentCLI(model); err != nil {
			return FixStartResult{}, err
		}
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
		case FixKindClickUp:
			hits = b.FindByClickUpIssue(project, opts.ClickUpID, opts.CustomID, opts.Identifier, false)
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

	return b.startFixCreate(project, cwd, tracked, prompt, opts, cli, model != "")
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
	case FixKindClickUp:
		custom := sessionstore.NormalizeClickUpCustomID(opts.CustomID)
		if custom == "" {
			custom = sessionstore.NormalizeClickUpCustomID(opts.Identifier)
		}
		return sessionstore.TrackedIssue{
			Provider:  sessionstore.ProviderClickUp,
			ClickUpID: strings.TrimSpace(opts.ClickUpID),
			CustomID:  custom,
			Title:     strings.TrimSpace(opts.Title),
			URL:       strings.TrimSpace(opts.URL),
			State:     strings.TrimSpace(opts.State),
			Keyword:   sessionstore.IssueKeywordFixes,
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
	case FixKindClickUp:
		display := strings.TrimSpace(opts.CustomID)
		if display == "" {
			display = strings.TrimSpace(opts.Identifier)
		}
		if display == "" {
			display = strings.TrimSpace(opts.ClickUpID)
		}
		return BuildClickUpFixPrompt(opts.Actor.DisplayName, display, opts.Title, opts.URL, opts.State, opts.Body)
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

func (b *Bot) startFixCreate(project, cwd string, tracked sessionstore.TrackedIssue, prompt string, opts FixStartOpts, cli config.AgentCLI, stampCLI bool) (FixStartResult, error) {
	bind := func(unitID, discordURL string) error {
		if err := b.bindFixIssue(unitID, project, tracked, opts.Actor, discordURL, true); err != nil {
			return err
		}
		if !stampCLI {
			return nil
		}
		return b.stampNewSessionCLI(unitID, cli)
	}
	// Prefer Discord thread when gateway or test threadAPI is available.
	// CreateWorkflowThread REST failure falls back to web-native (session may be
	// non-nil after Register even when Discord API is down — see DiscordReady).
	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if errors.Is(err, config.ErrNoDiscordChannel) {
			// A project with no mapped channel is a normal state, not a failure:
			// go web-native, matching StartWebTask and StartCase.
			return b.startWebNativeUnit(project, cwd, prompt, KindTask, opts.Actor, nil, func(unitID string) error {
				return bind(unitID, "")
			})
		}
		if err != nil {
			// Broken channel config (ambiguous map, or discordChannelId not in it)
			// is an operator error — surface it rather than degrade silently.
			return FixStartResult{}, err
		}
		title := fixThreadTitle(tracked, opts)
		starter := fixStarterContent(tracked, opts.Actor)
		threadID, err := b.CreateWorkflowThread(channelID, title, starter)
		if err != nil {
			log.Printf("fix: create Discord thread failed project=%s: %v — web-native fallback", project, err)
			return b.startWebNativeUnit(project, cwd, prompt, KindTask, opts.Actor, nil, func(unitID string) error {
				return bind(unitID, "")
			})
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := bind(threadID, discordURL); err != nil {
			return FixStartResult{}, err
		}
		return b.startWebTask(threadID, project, cwd, prompt, KindTask, opts.Actor, discordURL, nil, true)
	}
	// No gateway/threadAPI: web-native unit (no createWorkflowThread).
	return b.startWebNativeUnit(project, cwd, prompt, KindTask, opts.Actor, nil, func(unitID string) error {
		return bind(unitID, "")
	})
}

// canCreateDiscordThread reports whether create may open a Discord workflow thread.
func (b *Bot) canCreateDiscordThread() bool {
	if b == nil {
		return false
	}
	return b.DiscordReady() || b.threadAPI != nil
}

// startWebNativeUnit allocates w_* + binds via bind, then StartTask (branch grok/web/ via unit id).
func (b *Bot) startWebNativeUnit(project, cwd, prompt string, kind Kind, actor Actor, attachmentPaths []string, bind func(unitID string) error) (FixStartResult, error) {
	unitID, err := b.allocWebNativeUnit(project, bind)
	if err != nil {
		return FixStartResult{}, err
	}
	return b.startWebTask(unitID, project, cwd, prompt, kind, actor, "", attachmentPaths, true)
}

// allocWebNativeUnit allocates a w_* unit id and binds metadata without starting
// a task (StartCase intake-only shells stop here; run paths continue to StartTask).
func (b *Bot) allocWebNativeUnit(project string, bind func(unitID string) error) (string, error) {
	unitID := gitworktree.NewWebUnitID()
	if bind != nil {
		if err := bind(unitID); err != nil {
			return "", err
		}
	}
	// Pre-seed WorktreeBranch so cleanup/list see web prefix before first Ensure completes.
	if b.sessions != nil {
		_, _, _ = b.sessions.Patch(unitID, func(ent *sessionstore.Entry) {
			if ent.WorktreeBranch == "" {
				ent.WorktreeBranch = gitworktree.BranchNameForUnit(unitID)
			}
			if ent.Project == "" {
				ent.Project = project
			}
			if ent.Origin == "" {
				ent.Origin = SourceWeb
			}
		})
	}
	return unitID, nil
}

func (b *Bot) startWebTask(threadID, project, cwd, prompt string, kind Kind, actor Actor, discordURL string, attachmentPaths []string, created bool) (FixStartResult, error) {
	pos, err := b.StartTask(StartTaskOpts{
		ThreadID:        threadID,
		Proj:            projectRef{Name: project, Cwd: cwd},
		Prompt:          prompt,
		Kind:            kind,
		Actor:           actor,
		Source:          SourceWeb,
		Origin:          SourceWeb,
		CreatedBy:       actor.ID,
		CreatedByName:   actor.DisplayName,
		DiscordURL:      discordURL,
		DG:              b.Discord(),
		AttachmentPaths: attachmentPaths,
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
		Created:    created,
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
