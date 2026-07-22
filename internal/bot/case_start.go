package bot

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// ErrEmptyCaseTitle rejects web case intake without a customer-facing title.
var ErrEmptyCaseTitle = errors.New("case title required")

// FixStatusOpened is returned by StartCase when the case shell was created
// without queuing a run (intake-only, like Discord "/case").
const FixStatusOpened FixStartStatus = "opened"

// StartCaseOpts opens a support case from the web — the web equivalent of
// "@Grok /case [severity] [ref:ID] <title>" (case_cmd.go).
type StartCaseOpts struct {
	Project  string
	Title    string // customer-facing title (required)
	Severity string // low|medium|high|critical (default medium)
	Ref      string // optional external ticket id (ZD-4821, ACME-231, …)
	Notes    string // optional intake notes; non-empty → queue an investigate run
	Actor    Actor
}

// StartCase creates a work unit and the case shell (Mode=case, Phase=intake).
// Destination parity with StartWebTask: Discord thread when the gateway is up
// and the project has a mapped channel; otherwise a web-native w_* unit, with
// DiscordOffline flagged only on the thread-create-failure fallback. Intake
// never runs Grok — when Notes is set, a KindStartInvestigate task is queued
// and snapshotPolicyOntoItem promotes intake → investigate before the snapshot
// (K19), exactly as Discord "/investigate" in a case thread.
func (b *Bot) StartCase(opts StartCaseOpts) (FixStartResult, error) {
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
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		return FixStartResult{}, ErrEmptyCaseTitle
	}
	severity := normalizeSeverity(opts.Severity)
	ref := strings.TrimSpace(opts.Ref)
	notes := strings.TrimSpace(opts.Notes)

	bind := func(threadID, discordURL string) error {
		if err := b.bindWebStartedSession(threadID, project, clampGoal(title), opts.Actor, discordURL, true); err != nil {
			return err
		}
		return b.ensureCaseShell(threadID, project, opts.Actor, severity, ref, title, SourceWeb)
	}
	// finish runs after the shell exists so the policy snapshot sees Mode=case.
	finish := func(threadID, discordURL string) (FixStartResult, error) {
		if notes == "" {
			return FixStartResult{
				Status:     FixStatusOpened,
				ThreadID:   threadID,
				DiscordURL: discordURL,
				Created:    true,
			}, nil
		}
		prompt := caseIntakePrompt(severity, ref, title, notes)
		return b.startWebTask(threadID, project, cwd, prompt, KindStartInvestigate, opts.Actor, discordURL, true)
	}
	webNative := func() (FixStartResult, error) {
		unitID, err := b.allocWebNativeUnit(project, func(id string) error { return bind(id, "") })
		if err != nil {
			return FixStartResult{}, err
		}
		return finish(unitID, "")
	}

	if b.canCreateDiscordThread() {
		channelID, err := b.cfg.PreferDiscordChannel(project)
		if err != nil {
			log.Printf("case: no Discord channel for project=%s: %v — web-native fallback", project, err)
			return webNative()
		}
		threadID, err := b.CreateWorkflowThread(channelID, clampThreadTitle("Case · "+title), caseStarterContent(opts.Actor, severity, ref, title))
		if err != nil {
			// Broken promise: the intake page advertised a Discord destination
			// (mirrors StartWebTask's thread-create-failure fallback).
			log.Printf("case: create Discord thread failed project=%s: %v — web-native fallback", project, err)
			res, err := webNative()
			if err == nil {
				res.DiscordOffline = true
			}
			return res, err
		}
		discordURL := DiscordThreadURL(b.cfg.ProjectDiscordGuildID(project), threadID)
		if err := bind(threadID, discordURL); err != nil {
			return FixStartResult{}, err
		}
		return finish(threadID, discordURL)
	}
	return webNative()
}

// caseStarterContent is the parent-channel message a web-opened case thread
// starts on: the same card Discord "/case" posts, so both intakes look alike.
func caseStarterContent(actor Actor, severity, ref, title string) string {
	who := actor.DisplayName
	if who == "" {
		who = actor.ID
	}
	if who == "" {
		who = "web"
	}
	return sanitizeDiscordContent(fmt.Sprintf("**Grok Work** · case · opened by %s (web)\n%s",
		who, formatCaseCard(severity, title, ref, sessionstore.PhaseIntake, "")))
}

// caseIntakePrompt frames the queued investigate run: a fresh headless session
// has no thread backlog to read, so the customer-facing context rides the prompt.
func caseIntakePrompt(severity, ref, title, notes string) string {
	var sb strings.Builder
	sb.WriteString("Support case (severity ")
	sb.WriteString(severity)
	if ref != "" {
		sb.WriteString(", ref ")
		sb.WriteString(ref)
	}
	sb.WriteString("): ")
	sb.WriteString(title)
	sb.WriteString("\n\nIntake notes:\n")
	sb.WriteString(notes)
	return sb.String()
}
