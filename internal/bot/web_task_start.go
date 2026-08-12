package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// StartWebTaskOpts starts a new work unit from a freeform web prompt — the web
// equivalent of "@Grok <task>" in a mapped channel.
type StartWebTaskOpts struct {
	Project string
	Prompt  string
	Actor   Actor
	// Title is an optional short sticky goal. When empty, Goal is derived with
	// threadNameFromPrompt (same local short name Discord uses for a new thread
	// before any async rename) — no model call.
	Title string
	Mode  string // "" | "fix" | "investigate" | "explain"
	// Model names the model this session should run on, stamping the agent that
	// owns it. Empty means "whatever config says", which stays unstamped so the
	// existing resolve-at-run-start path applies. Requires builder-class caps.
	Model string
	// AttachmentPaths are staged web images; ownership transfers to StartTask.
	AttachmentPaths []string
	// WebNative forces a w_* unit on grok/web/. When true, skip Discord thread
	// create even if the gateway is up and a channel is mapped.
	WebNative bool
}

// StartWebTask creates a workflow unit and enqueues a freeform Grok task.
// A Discord thread is opened when the gateway is up and the project has a mapped
// channel; otherwise (gateway down, thread-create failure, or no mapped channel)
// it falls back to a web-native w_* unit on grok/web/. The thread-create-failure
// fallback additionally sets DiscordOffline on the result (the page had promised
// a Discord destination); the other two fallbacks do not. Ship mode, the worktree,
// and the remote-work contract are all applied at execute time (executeTask), so
// there are no ship/worktree/contract parameters here.
//
// Sticky Goal: explicit Title wins; otherwise Goal is the same short local name
// used for a Discord thread title (threadNameFromPrompt), not the full prompt
// and not a SummarizeTitle model call.
func (b *Bot) StartWebTask(opts StartWebTaskOpts) (FixStartResult, error) {
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
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return FixStartResult{}, ErrEmptyPrompt
	}
	userTitle := strings.TrimSpace(opts.Title)
	titleSrc := userTitle
	if titleSrc == "" {
		titleSrc = prompt
	}
	// Explicit Title is the Goal as typed. Blank Title → short local name, the
	// same derivation Discord uses for the first thread title paint.
	goal := clampGoal(userTitle)
	if goal == "" {
		goal = clampGoal(threadNameFromPrompt(prompt, opts.Actor.DisplayName))
	}
	kind := webTaskKind(opts.Mode)
	// Hard-block Fix & ship without builder-class caps (no silent coerce).
	if wantsFixStartMode(opts.Mode, b.cfg.ProjectDefaultMode(project)) {
		if err := b.requireCanStartFix(project, opts.Actor.ID); err != nil {
			return FixStartResult{}, err
		}
	}
	// Same stance for a named model: reject rather than silently downgrade to the
	// default, so a forged form value is visible instead of ignored.
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

	bind := func(threadID, discordURL string) error {
		if err := b.bindWebStartedSession(threadID, project, goal, opts.Actor, discordURL, true); err != nil {
			return err
		}
		if model == "" {
			return nil
		}
		return b.stampNewSessionCLI(threadID, cli)
	}

	if opts.WebNative {
		return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
			return bind(unitID, "")
		})
	}

	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if errors.Is(err, config.ErrNoDiscordChannel) {
			// Freeform web starts must not require a mapped Discord channel: fall back
			// to a web-native unit. (A freeform start is the one web path that still
			// prefers a Discord thread at all — the commit-review and PR dispatch cards
			// are always web-native.)
			return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
				return bind(unitID, "")
			})
		}
		if err != nil {
			// Broken channel config, not an absent one: surface it instead of
			// quietly making every session for this project web-native.
			return FixStartResult{}, err
		}
		title := threadNameFromPrompt(titleSrc, opts.Actor.DisplayName)
		starter := webTaskStarter(opts.Actor)
		threadID, err := b.CreateWorkflowThread(channelID, title, starter)
		if err != nil {
			// Broken promise: the start page advertised a Discord destination, but the
			// thread create failed. Fall back web-native and flag DiscordOffline so the
			// session page surfaces the "discord=offline" flash. (The no-mapped-channel
			// and gateway-down branches already showed "web-native" and do not flag.)
			log.Printf("web-task: create Discord thread failed project=%s: %v — web-native fallback", project, err)
			res, err := b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
				return bind(unitID, "")
			})
			if err == nil {
				res.DiscordOffline = true
			}
			return res, err
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := bind(threadID, discordURL); err != nil {
			return FixStartResult{}, err
		}
		return b.startWebTask(threadID, project, cwd, prompt, kind, opts.Actor, discordURL, opts.AttachmentPaths, true)
	}
	return b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
		return bind(unitID, "")
	})
}

// webTaskKind maps the start-form mode select onto a task Kind, mirroring Discord
// "/start fix|investigate|explain". "fix" maps to KindStartFix so the web can
// force fix-mode ship even on a project whose default mode is non-ship (snapshot
// stamps ModeFix). "" stays KindTask → the project default (cfg.ProjectDefaultMode).
func webTaskKind(mode string) Kind {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeFix:
		return KindStartFix
	case ModeInvestigate:
		return KindStartInvestigate
	case ModeExplain:
		return KindStartExplain
	default:
		return KindTask
	}
}

// WantsFixStartMode reports whether a start mode (or project default when
// mode is empty) requests Fix & ship. Exported for the machine API gate.
func WantsFixStartMode(mode, projectDefault string) bool {
	return wantsFixStartMode(mode, projectDefault)
}

// wantsFixStartMode reports whether a web start form mode (or project default
// when mode is empty) requests Fix & ship.
func wantsFixStartMode(mode, projectDefault string) bool {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = strings.ToLower(strings.TrimSpace(projectDefault))
	}
	if m == "" {
		m = ModeFix
	}
	return m == ModeFix
}

// requireCanStartFix fails when the actor lacks CanShip (startSessions +
// githubWrites). Used by web StartWebTask / StartFix and Discord /start fix.
// Nil config matches ResolveCapabilities (builder default for unmapped legacy).
func (b *Bot) requireCanStartFix(project, userID string) error {
	if b == nil {
		return ErrCannotStartFix
	}
	caps := config.BuiltinCapabilityTemplates["builder"]
	if b.cfg != nil {
		caps = b.cfg.ResolveCapabilities(project, userID)
	}
	if !caps.CanShip() {
		return ErrCannotStartFix
	}
	return nil
}

// bindWebStartedSession stamps workflow metadata + owner onto a web-started unit.
// Shared by StartWebTask and StartCommitReview. The owner stamp is critical:
// startWebNativeUnit's pre-seed sets no owner, so without this the creator would
// be locked out of their own cancel/reset.
func (b *Bot) bindWebStartedSession(threadID, project, goal string, actor Actor, discordURL string, isNew bool) error {
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

func webTaskStarter(actor Actor) string {
	who := actor.DisplayName
	if who == "" {
		who = actor.ID
	}
	if who == "" {
		who = "web"
	}
	return fmt.Sprintf("**Grok Work** · task · started by %s (web)", who)
}
