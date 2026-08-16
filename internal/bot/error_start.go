package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// ErrorIntent selects Investigate vs Fix for StartError.
type ErrorIntent string

const (
	ErrorIntentInvestigate ErrorIntent = "investigate"
	ErrorIntentFix         ErrorIntent = "fix"
)

// ErrInvalidError is a missing/malformed production-error locator.
var ErrInvalidError = errors.New("invalid error")

// ErrErrorSourceDisabled is returned when the project's provider is off.
var ErrErrorSourceDisabled = errors.New("error source not enabled")

// ErrorStartOpts starts or reuses a work unit from a production error group.
type ErrorStartOpts struct {
	Provider    string
	Intent      ErrorIntent
	Project     string
	Actor       Actor
	ForceNew    bool
	ThreadID    string
	ID          string
	ShortID     string
	Title       string
	URL         string
	Status      string
	Fingerprint string
	Location    string
	Resource    string
	Count       int64
	LastSeen    time.Time
	Model       string
}

// StartError discovers or creates a work unit, binds the error, and StartTasks.
// Investigate enqueues KindStartInvestigate; Fix enqueues KindStartFix.
func (b *Bot) StartError(opts ErrorStartOpts) (FixStartResult, error) {
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
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if err := b.requireErrorProvider(project, provider); err != nil {
		return FixStartResult{}, err
	}
	if strings.TrimSpace(opts.ID) == "" && strings.TrimSpace(opts.ShortID) == "" {
		return FixStartResult{}, ErrInvalidError
	}
	if provider == errsrc.ProviderDeploys {
		if strings.TrimSpace(opts.Location) == "" || strings.TrimSpace(opts.Resource) == "" || strings.TrimSpace(opts.ID) == "" {
			return FixStartResult{}, ErrInvalidError
		}
	}
	intent := opts.Intent
	if intent == "" {
		intent = ErrorIntentInvestigate
	}
	if intent == ErrorIntentFix {
		if err := b.requireCanStartFix(project, opts.Actor.ID); err != nil {
			return FixStartResult{}, err
		}
	}

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

	tracked := errorTracked(opts)
	prompt := errorPromptFor(opts, tracked)
	kind := errorKind(intent)

	if tid := strings.TrimSpace(opts.ThreadID); tid != "" && !opts.ForceNew {
		return b.startErrorReuse(tid, project, cwd, tracked, prompt, kind, opts.Actor)
	}

	if !opts.ForceNew {
		hits := b.FindByError(project, provider, opts.ID, opts.Location, opts.Resource)
		switch len(hits) {
		case 0:
		case 1:
			return b.startErrorReuse(hits[0].ThreadID, project, cwd, tracked, prompt, kind, opts.Actor)
		default:
			return FixStartResult{Status: FixStatusPicker, Hits: hits}, ErrPickerRequired
		}
	}

	return b.startErrorCreate(project, cwd, tracked, prompt, kind, opts, cli, model != "")
}

func (b *Bot) requireErrorProvider(project, provider string) error {
	if b == nil || b.cfg == nil {
		return ErrErrorSourceDisabled
	}
	switch provider {
	case errsrc.ProviderDeploys:
		if !b.cfg.ProjectDeploysErrorsEnabled(project) {
			return ErrErrorSourceDisabled
		}
	case errsrc.ProviderSentry:
		if !b.cfg.ProjectSentryEnabled(project) {
			return ErrErrorSourceDisabled
		}
	case errsrc.ProviderGCP:
		if !b.cfg.ProjectGCPErrorsEnabled(project) {
			return ErrErrorSourceDisabled
		}
	default:
		return ErrInvalidError
	}
	return nil
}

func errorKind(intent ErrorIntent) Kind {
	if intent == ErrorIntentFix {
		return KindStartFix
	}
	return KindStartInvestigate
}

func errorTracked(opts ErrorStartOpts) sessionstore.TrackedError {
	last := ""
	if !opts.LastSeen.IsZero() {
		last = opts.LastSeen.UTC().Format(time.RFC3339)
	}
	return sessionstore.TrackedError{
		Provider:    strings.ToLower(strings.TrimSpace(opts.Provider)),
		ID:          strings.TrimSpace(opts.ID),
		ShortID:     strings.TrimSpace(opts.ShortID),
		Title:       strings.TrimSpace(opts.Title),
		URL:         strings.TrimSpace(opts.URL),
		Status:      strings.TrimSpace(opts.Status),
		Count:       opts.Count,
		LastSeen:    last,
		Fingerprint: strings.TrimSpace(opts.Fingerprint),
		Location:    strings.TrimSpace(opts.Location),
		Resource:    strings.TrimSpace(opts.Resource),
	}
}

func errorPromptFor(opts ErrorStartOpts, tracked sessionstore.TrackedError) string {
	if opts.Intent == ErrorIntentFix {
		return BuildErrorFixPrompt(opts.Actor.DisplayName, tracked)
	}
	return BuildErrorInvestigatePrompt(opts.Actor.DisplayName, tracked)
}

func (b *Bot) startErrorReuse(threadID, project, cwd string, tracked sessionstore.TrackedError, prompt string, kind Kind, actor Actor) (FixStartResult, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return FixStartResult{}, fmt.Errorf("empty thread id")
	}
	if err := b.bindError(threadID, project, tracked, actor, "", false); err != nil {
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
		Kind:          kind,
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

func (b *Bot) startErrorCreate(project, cwd string, tracked sessionstore.TrackedError, prompt string, kind Kind, opts ErrorStartOpts, cli config.AgentCLI, stampCLI bool) (FixStartResult, error) {
	bind := func(unitID, discordURL string) error {
		if err := b.bindError(unitID, project, tracked, opts.Actor, discordURL, true); err != nil {
			return err
		}
		if !stampCLI {
			return nil
		}
		return b.stampNewSessionCLI(unitID, cli)
	}
	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if errors.Is(err, config.ErrNoDiscordChannel) {
			return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, nil, func(unitID string) error {
				return bind(unitID, "")
			})
		}
		if err != nil {
			return FixStartResult{}, err
		}
		title := errorThreadTitle(tracked, opts)
		starter := errorStarterContent(tracked, opts.Actor, opts.Intent)
		threadID, err := b.CreateWorkflowThread(channelID, title, starter)
		if err != nil {
			log.Printf("error-start: create Discord thread failed project=%s: %v — web-native fallback", project, err)
			return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, nil, func(unitID string) error {
				return bind(unitID, "")
			})
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := bind(threadID, discordURL); err != nil {
			return FixStartResult{}, err
		}
		return b.startWebTask(threadID, project, cwd, prompt, kind, opts.Actor, discordURL, nil, true)
	}
	return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, nil, func(unitID string) error {
		return bind(unitID, "")
	})
}

func (b *Bot) bindError(threadID, project string, tracked sessionstore.TrackedError, actor Actor, discordURL string, isNew bool) error {
	if b.sessions == nil {
		return fmt.Errorf("sessions store nil")
	}
	goal := errorGoal(tracked)
	var upsertErr error
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
		if isNew || strings.TrimSpace(ent.Goal) == "" {
			ent.Goal = goal
		}
		if actor.ID != "" {
			ensureSessionOwner(ent, actor.ID, actor.String())
		}
		upsertErr = ent.UpsertError(tracked)
	})
	if err != nil {
		return err
	}
	if ok {
		return upsertErr
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
	if err := e.UpsertError(tracked); err != nil {
		return err
	}
	return b.sessions.Set(threadID, e)
}

// FindByError returns candidate units that bind this error group (same project).
// PR-review units and terminal labels are excluded — reuse continues diagnosis/fix.
func (b *Bot) FindByError(project, provider, id, location, resource string) []IssueSessionHit {
	if b == nil || b.sessions == nil {
		return nil
	}
	project = strings.TrimSpace(project)
	target := sessionstore.TrackedError{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		ID:       strings.TrimSpace(id),
		Location: strings.TrimSpace(location),
		Resource: strings.TrimSpace(resource),
	}
	if project == "" || target.Provider == "" || (target.ID == "" && target.ShortID == "") {
		return nil
	}
	var hits []IssueSessionHit
	for _, listed := range b.sessions.List() {
		if !strings.EqualFold(listed.Project, project) {
			continue
		}
		if listed.IsPRReview() {
			continue
		}
		if sessionstore.IsTerminalLabel(listed.EffectiveLabel()) {
			continue
		}
		if !entryBindsError(listed.Entry, target) {
			continue
		}
		qlen := b.queueLen(listed.ThreadID)
		busy := false
		if _, ok := b.getJob(listed.ThreadID); ok {
			busy = true
		} else if qlen > 0 {
			busy = true
		}
		hits = append(hits, IssueSessionHit{
			ThreadID:    listed.ThreadID,
			Project:     listed.Project,
			Goal:        listed.Goal,
			Label:       listed.EffectiveLabel(),
			OwnerName:   listed.OwnerName,
			OwnerID:     listed.OwnerID,
			UpdatedAt:   listed.UpdatedAt,
			Busy:        busy,
			QueueLen:    qlen,
			HasWorktree: strings.TrimSpace(listed.WorktreeBranch) != "" || strings.TrimSpace(listed.Cwd) != "",
			DiscordURL:  listed.DiscordURL,
			SessionKind: listed.SessionKind,
		})
	}
	sortIssueHits(hits)
	return hits
}

// preserveErrorFields copies bound production errors when session Set overwrites the entry.
func preserveErrorFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if len(next.Errors) == 0 && len(prev.Errors) > 0 {
		next.Errors = append([]sessionstore.TrackedError(nil), prev.Errors...)
	}
}

func entryBindsError(e sessionstore.Entry, target sessionstore.TrackedError) bool {
	for _, err := range e.Errors {
		if sessionstore.SameError(err, target) {
			return true
		}
	}
	return false
}

func errorThreadTitle(tracked sessionstore.TrackedError, opts ErrorStartOpts) string {
	summary := strings.TrimSpace(opts.Title)
	if summary == "" {
		summary = errorGoal(tracked)
	}
	name := threadNameFromPrompt(summary, opts.Actor.DisplayName)
	if len(name) > 100 {
		name = name[:97] + "…"
	}
	return name
}

func errorStarterContent(tracked sessionstore.TrackedError, actor Actor, intent ErrorIntent) string {
	who := actor.DisplayName
	if who == "" {
		who = actor.ID
	}
	if who == "" {
		who = "web"
	}
	verb := "Investigate"
	if intent == ErrorIntentFix {
		verb = "Fix"
	}
	line := fmt.Sprintf("**Grok Work** · %s %s · started by %s (web)", verb, tracked.DisplayRef(), who)
	if u := strings.TrimSpace(tracked.URL); u != "" {
		line += "\n" + u
	}
	return line
}
