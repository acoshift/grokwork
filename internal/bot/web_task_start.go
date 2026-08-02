package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// StartWebTaskOpts starts a new work unit from a freeform web prompt — the web
// equivalent of "@Grok <task>" in a mapped channel.
type StartWebTaskOpts struct {
	Project string
	Prompt  string
	Actor   Actor
	Title   string // optional short title/goal; when empty, SummarizeTitle fills Goal
	Mode    string // "" | "fix" | "investigate" | "explain"
	// Model names the model this session should run on, stamping the agent that
	// owns it. Empty means "whatever config says", which stays unstamped so the
	// existing resolve-at-run-start path applies. Requires builder-class caps.
	Model string
	// AttachmentPaths are staged web images; ownership transfers to StartTask.
	AttachmentPaths []string
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
// When Title is empty, Goal is provisionally the prompt (clamped) and, when
// summarize is enabled, improved asynchronously via SummarizeTitle — the same
// off-critical-path call Discord uses to name a new thread.
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
	// Prefer the optional Title for the session goal too (not just the Discord
	// thread name) so it surfaces on the session page/list on both destinations.
	// When Title is blank, Goal starts as the prompt and may be replaced by the
	// async summarize path below (Discord's improveThreadTitle equivalent).
	goal := clampGoal(prompt)
	if userTitle != "" {
		goal = clampGoal(userTitle)
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

	// finish wraps every successful start so a blank Title still gets a short
	// Goal (and Discord thread name) without blocking the redirect.
	finish := func(res FixStartResult, err error) (FixStartResult, error) {
		if err != nil {
			return res, err
		}
		if userTitle == "" {
			b.scheduleImproveWebTaskGoal(res.ThreadID, prompt, opts.Actor.DisplayName, cwd, goal)
		}
		return res, nil
	}

	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if errors.Is(err, config.ErrNoDiscordChannel) {
			// Freeform web starts must not require a mapped Discord channel: fall back
			// to a web-native unit. (A freeform start is the one web path that still
			// prefers a Discord thread at all — the commit-review and PR dispatch cards
			// are always web-native.)
			return finish(b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
				return bind(unitID, "")
			}))
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
			return finish(res, err)
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := bind(threadID, discordURL); err != nil {
			return FixStartResult{}, err
		}
		return finish(b.startWebTask(threadID, project, cwd, prompt, kind, opts.Actor, discordURL, opts.AttachmentPaths, true))
	}
	return finish(b.startWebNativeUnit(project, cwd, prompt, kind, opts.Actor, opts.AttachmentPaths, func(unitID string) error {
		return bind(unitID, "")
	}))
}

// scheduleImproveWebTaskGoal kicks off SummarizeTitle when the start form left
// Title blank. No-op when summarize is disabled or the unit id is empty.
func (b *Bot) scheduleImproveWebTaskGoal(threadID, prompt, username, cwd, provisionalGoal string) {
	if b == nil || b.cfg == nil || !b.cfg.SummarizeTitleEnabled() {
		return
	}
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(prompt) == "" {
		return
	}
	go b.improveWebTaskGoal(threadID, prompt, username, cwd, provisionalGoal)
}

// improveWebTaskGoal is StartWebTask's counterpart of improveThreadTitle: one
// tools-off summarize turn produces a short sticky Goal. When the unit is a
// Discord thread it is also renamed. The provisional Goal (the prompt) stays
// until this succeeds, and a Goal the user already edited is left alone.
func (b *Bot) improveWebTaskGoal(threadID, prompt, username, cwd, provisionalGoal string) {
	if b == nil || b.cfg == nil || threadID == "" {
		return
	}
	if b.stopping.Load() {
		return
	}
	timeout := time.Duration(b.cfg.SummarizeTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	log.Printf("web-task: summarizing goal async unit=%s…", threadID)
	sumCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cli := b.threadSummarizeCLI(threadID).CLI()
	log.Printf("web-task: summarize agent=%s model=%q unit=%s", cli.Agent, cli.Model, threadID)
	t, ok := grokrun.SummarizeTitle(sumCtx, cli, prompt, cwd, timeout)
	if !ok {
		log.Printf("web-task: async summarize failed unit=%s (keeping provisional goal)", threadID)
		return
	}
	if b.stopping.Load() {
		return
	}
	issues := sessionstore.ParseIssueRefs(prompt)
	name := prefixThreadTitleWithIssues(threadNameFromPrompt(t, username), issues)
	goal := clampGoal(name)
	if goal == "" {
		return
	}
	provisional := strings.TrimSpace(provisionalGoal)
	if b.sessions != nil {
		if _, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			cur := strings.TrimSpace(ent.Goal)
			// Only replace the auto provisional (or empty). A hand-set Title or a
			// later /goal edit must not be clobbered by a late summarize.
			if cur == "" || cur == provisional {
				ent.Goal = goal
			}
		}); err != nil {
			log.Printf("warn: async set goal unit=%s: %v", threadID, err)
		} else {
			log.Printf("web-task: async goal unit=%s → %q", threadID, goal)
		}
	}
	// Web-native units have no Discord channel to rename.
	if gitworktree.IsWebUnitID(threadID) {
		return
	}
	s := b.Discord()
	if s == nil {
		return
	}
	if _, err := s.ChannelEdit(threadID, &discordgo.ChannelEdit{Name: name}); err != nil {
		log.Printf("warn: async retitle web-started thread=%s: %v", threadID, err)
		return
	}
	log.Printf("web-task: async retitle thread=%s → %q", threadID, name)
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
