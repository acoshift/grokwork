package bot

import (
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Session mode values (Wave 1).
const (
	ModeInvestigate = "investigate"
	ModeExplain     = "explain"
	ModeFix         = "fix"
	// ModeCase is Wave 3; recognized for freeform inherit if set later.
	ModeCase = "case"
)

// RunKind values recorded on history turns and journal snapshots.
const (
	RunKindFix         = "fix"
	RunKindInvestigate = "investigate"
	RunKindExplain     = "explain"
	RunKindFixCI       = "fix_ci"
	RunKindAddress     = "address"
	RunKindPreset      = "preset"
)

// RunPolicy is the bot-enforced gate set for one Grok child run (K2).
type RunPolicy struct {
	Mode                 string
	Phase                string
	RunKind              string
	AllowPR              bool
	AllowDirectShip      bool
	Yolo                 bool
	Tools                *string // nil unrestricted; non-nil → allowlist / tools-off
	NoSubagents          bool
	IncludeGHToken       bool
	PrefixKind           string // "remote" | "investigate" | "explain" | "none"
	RefreshPR            bool
	RefreshPRWarnOnly    bool
	PostCompletion       string // "eng" | "dossier" | "none"
	RefreshBrief         bool
	AllowUpload          bool
	AllowDirectIntegrate bool
	DirtyTreeWarn        bool
	Coerced              bool // StartSessions without GithubWrites coerced to investigate
	// InvestigateShell is true when the investigate allowlist includes host shell
	// (role-gated: SafeOps or CanShip). Used for prompt contract, not CLI flags.
	InvestigateShell bool
}

// PolicyInput is the pure decision input for BuildRunPolicy.
type PolicyInput struct {
	SessionMode  string // Entry.Mode or empty
	SessionPhase string // Wave 3; empty in Wave 1
	ShipMode     string // sessionstore.ShipModePR | ShipModeDirect | ""
	Caps         config.Capabilities
	ConfigYolo   bool
	// RequestedMode from /start or freeform inherit; empty → session or fix default.
	RequestedMode string
	// RequestedRunKind optional explicit kind (fix_ci, address).
	RequestedRunKind string
	// ForceInvestigate forces investigate policy (e.g. /start investigate).
	ForceInvestigate bool
	// SafeTeamMode on project (affects nothing here; caps already resolved).
	InvestigateTools string // project override; empty → default read allowlist
	// Agent is the coding CLI this run will use. Tool names are agent-specific
	// vocabulary, so the investigate allowlist depends on it.
	Agent grokrun.Agent
}

// DefaultInvestigateTools is the file-only Wave 1 allowlist (K21) for grok.
// Prefer grokrun.Agent.InvestigateTools; this constant is kept for older call sites.
const DefaultInvestigateTools = "read_file,grep"

// investigateTools picks the allowlist for an investigate run.
//
// Shell is role-gated: only SafeOps or builder-class (CanShip) actors get host
// shell tools. Plain investigators stay file-only.
//
// The project override is written in one agent's tool vocabulary and only
// applies when shell is allowed (otherwise an override that listed Bash would
// re-open host shell for support). Handing grok names to claude (or the reverse)
// would resolve to an allowlist of zero real tools, so an override only applies
// to the agent whose names it is written in; other agents get their own default.
func investigateTools(agent grokrun.Agent, override string, caps config.Capabilities) string {
	shell := caps.CanInvestigateShell()
	if !shell {
		return agent.InvestigateTools(false)
	}
	override = strings.TrimSpace(override)
	if override != "" && agent.Resolve() == grokrun.AgentGrok {
		return override
	}
	return agent.InvestigateTools(true)
}

// BuildRunPolicy is a pure function: mode × caps × ship → gates (testable without Discord).
func BuildRunPolicy(in PolicyInput) RunPolicy {
	mode := strings.TrimSpace(strings.ToLower(in.RequestedMode))
	if mode == "" {
		mode = strings.TrimSpace(strings.ToLower(in.SessionMode))
	}
	if in.ForceInvestigate {
		mode = ModeInvestigate
	}

	// D2: without GithubWrites cannot ship (never half-fix).
	// Keep Mode=case when already a case (K17); only drop to ModeInvestigate for non-case.
	coerced := false
	if !in.ForceInvestigate && mode != ModeInvestigate && mode != ModeExplain && !in.Caps.GithubWrites {
		wantShip := mode == "" || mode == ModeFix || mode == ModeCase
		if wantShip {
			if mode == ModeCase {
				// Stay case; force non-ship phase for this policy decision.
				if !isCaseNonShipPhase(strings.TrimSpace(strings.ToLower(in.SessionPhase))) {
					in.SessionPhase = sessionstore.PhaseInvestigate
				}
				coerced = true
			} else {
				mode = ModeInvestigate
				coerced = true
			}
		}
	}

	rk := strings.TrimSpace(strings.ToLower(in.RequestedRunKind))
	if rk == "" {
		switch mode {
		case ModeInvestigate:
			rk = RunKindInvestigate
		case ModeExplain:
			rk = RunKindExplain
		default:
			rk = RunKindFix
		}
	}

	phase := strings.TrimSpace(strings.ToLower(in.SessionPhase))

	// Case closed: PrefixKind none + investigate-grade gates (defense if a run still starts).
	if mode == ModeCase && phase == sessionstore.PhaseClosed {
		empty := ""
		return RunPolicy{
			Mode: ModeCase, Phase: phase, RunKind: rk,
			AllowPR: false, AllowDirectShip: false, AllowDirectIntegrate: false,
			Yolo: false, Tools: &empty, NoSubagents: true, IncludeGHToken: false,
			PrefixKind: "none", PostCompletion: "none",
			RefreshPR: false, RefreshBrief: false, AllowUpload: false,
			DirtyTreeWarn: false, Coerced: coerced,
		}
	}

	// Explicit investigate/explain run kinds stay non-ship even on fixing/shipping cases.
	// Case non-ship phases + investigate/explain modes: non-shipping (K27).
	caseNonShip := mode == ModeCase && (isCaseNonShipPhase(phase) || rk == RunKindInvestigate || rk == RunKindExplain)
	if mode == ModeInvestigate || mode == ModeExplain || caseNonShip {
		if mode == ModeCase {
			// Keep Mode=case; phase stays; run kind investigate unless explicit explain
			if rk == RunKindFix || rk == "" {
				rk = RunKindInvestigate
			}
		}
		toolsCopy := investigateTools(in.Agent, in.InvestigateTools, in.Caps)
		pol := RunPolicy{
			Mode:                 mode,
			Phase:                phase,
			RunKind:              rk,
			AllowPR:              false,
			AllowDirectShip:      false,
			Yolo:                 false,
			Tools:                &toolsCopy,
			NoSubagents:          true,
			IncludeGHToken:       false,
			PrefixKind:           "investigate",
			RefreshPR:            false,
			RefreshPRWarnOnly:    true,
			PostCompletion:       "dossier",
			RefreshBrief:         false,
			AllowUpload:          false,
			AllowDirectIntegrate: false,
			DirtyTreeWarn:        true,
			Coerced:              coerced,
			InvestigateShell:     toolsListHasShell(in.Agent, toolsCopy),
		}
		if mode == ModeExplain || phase == sessionstore.PhaseAnswered {
			pol.PrefixKind = "explain"
			pol.PostCompletion = "none"
			empty := ""
			pol.Tools = &empty // tools-off rewrite
			pol.InvestigateShell = false
		}
		return pol
	}

	// Fix / empty mode / case fixing|shipping: ship-capable when GithubWrites.
	canWrite := in.Caps.GithubWrites
	// When SafeTeamMode off, ResolveCapabilities returns builder — CanShip true.
	// Explicit zero caps (denied) → treat as investigate fail-closed.
	if !canWrite && !in.Caps.StartSessions {
		return BuildRunPolicy(PolicyInput{
			SessionMode:      ModeInvestigate,
			ForceInvestigate: true,
			ConfigYolo:       in.ConfigYolo,
			Caps:             in.Caps,
			InvestigateTools: in.InvestigateTools,
			ShipMode:         in.ShipMode,
			Agent:            in.Agent,
		})
	}

	// Case ship phases: Mode stays case (K17).
	shipMode := strings.TrimSpace(in.ShipMode)
	direct := shipMode == sessionstore.ShipModeDirect
	pol := RunPolicy{
		Mode:                 mode,
		Phase:                phase,
		RunKind:              rk,
		AllowPR:              canWrite && !direct,
		AllowDirectShip:      canWrite && direct,
		Yolo:                 in.ConfigYolo,
		Tools:                nil, // unrestricted
		NoSubagents:          false,
		IncludeGHToken:       canWrite,
		PrefixKind:           "remote",
		RefreshPR:            canWrite && !direct,
		RefreshPRWarnOnly:    false,
		PostCompletion:       "eng",
		RefreshBrief:         true,
		AllowUpload:          true,
		AllowDirectIntegrate: canWrite && direct,
		DirtyTreeWarn:        false,
		Coerced:              coerced,
	}
	// PR mode with writes: AllowPR true even if shipMode empty (legacy PR default).
	if canWrite && !direct {
		pol.AllowPR = true
		pol.AllowDirectShip = false
		pol.RefreshPR = true
		pol.AllowDirectIntegrate = false
		pol.IncludeGHToken = true
	}
	if canWrite && direct {
		pol.AllowPR = false
		pol.AllowDirectShip = true
		pol.RefreshPR = false
		pol.AllowDirectIntegrate = true
		pol.IncludeGHToken = true
	}
	return pol
}

func isCaseNonShipPhase(phase string) bool {
	switch strings.TrimSpace(strings.ToLower(phase)) {
	case "", sessionstore.PhaseIntake, sessionstore.PhaseInvestigate, sessionstore.PhaseAnswered, sessionstore.PhaseClosed:
		return true
	default:
		return false
	}
}

// EscalationPackage builds the fix-run preamble for escalated cases.
func EscalationPackage(e sessionstore.Entry) string {
	var b strings.Builder
	b.WriteString("ESCALATION PACKAGE (case → eng fix on the same branch/worktree):\n")
	if e.CustomerTitle != "" {
		b.WriteString("- Customer title: ")
		b.WriteString(e.CustomerTitle)
		b.WriteString("\n")
	}
	if e.Severity != "" {
		b.WriteString("- Severity: ")
		b.WriteString(e.Severity)
		b.WriteString("\n")
	}
	if e.CustomerRef != "" {
		b.WriteString("- Customer ref: ")
		b.WriteString(e.CustomerRef)
		b.WriteString("\n")
	}
	if e.Dossier != nil && e.Dossier.Summary != "" {
		b.WriteString("- Investigation summary: ")
		b.WriteString(e.Dossier.Summary)
		b.WriteString("\n")
	}
	if e.Dossier != nil && len(e.Dossier.NextActions) > 0 {
		b.WriteString("- Suggested next actions: ")
		b.WriteString(strings.Join(e.Dossier.NextActions, "; "))
		b.WriteString("\n")
	}
	if e.ReporterName != "" {
		b.WriteString("- Reporter: ")
		b.WriteString(e.ReporterName)
		b.WriteString("\n")
	}
	if e.DiscordURL != "" {
		b.WriteString("- Discord: ")
		b.WriteString(e.DiscordURL)
		b.WriteString("\n")
	}
	b.WriteString("- Convert this case to a code fix on the SAME branch/worktree; do not create a parallel investigation.\n")
	b.WriteString("- Mode stays case; do not abandon support context.\n\n")
	return b.String()
}

// toolsListHasShell reports whether the comma allowlist includes this agent's shell tool.
func toolsListHasShell(agent grokrun.Agent, tools string) bool {
	sh := agent.ShellInvestigateTool()
	if sh == "" || strings.TrimSpace(tools) == "" {
		return false
	}
	for _, part := range strings.Split(tools, ",") {
		if strings.TrimSpace(part) == sh {
			return true
		}
	}
	return false
}

// investigatePromptPrefix is the non-shipping contract (no PR, no direct ship).
// shell is true when the actor's role granted diagnostic host shell tools.
func investigatePromptPrefix(branch string, shell bool) string {
	lines := []string{
		"You are investigating code on a shared workflow unit (Discord thread and/or web session).",
		"Mode: INVESTIGATE (read-only intent). Do NOT commit, push, open a pull request, or modify the remote.",
		"Do NOT run `gh pr create`, do NOT push to main/master, and do NOT merge.",
	}
	if shell {
		lines = append(lines,
			"You may use the shell for diagnostics: read logs, run status commands, query databases (e.g. psql SELECT), curl health endpoints, inspect processes.",
			"Prefer non-destructive commands. Do NOT mutate production data, drop tables, rewrite config, or edit application source as a \"fix\".",
		)
	} else {
		lines = append(lines,
			"You have file-inspection tools only (no shell). Prefer reading code and summarizing root cause.",
		)
	}
	lines = append(lines,
		"Explain findings in plain language.",
		"If you need a code change, say so and stop — a human will start a fix run.",
		"Do not claim the issue is fixed unless you only confirmed existing behavior.",
		"",
		"Filesystem scope: stay inside this unit's cwd/worktree and the project repo for code inspection.",
		"Do NOT scan the user's home directory or protected folders for secrets.",
		"",
	)
	if branch != "" {
		lines = append([]string{
			"Isolated git worktree for this workflow unit / thread.",
			"Branch: " + branch + " (do not push).",
			"",
		}, lines...)
	}
	return strings.Join(lines, "\n")
}

// explainPromptPrefix is customer-safe draft mode.
func explainPromptPrefix() string {
	return strings.Join([]string{
		"Mode: EXPLAIN — draft a customer-safe explanation only.",
		"No code changes, no commits, no PRs, no shell that mutates the repo.",
		"End with a CUSTOMER_UPDATE: block of plain language (no file paths, no SHAs, no secrets).",
		"",
	}, "\n")
}

// AttributionInput is pure input for Tier A ship attribution (no I/O).
type AttributionInput struct {
	PrompterName string // Discord display / Actor.String()
	PrompterID   string // Discord snowflake
	ThreadURL    string // Discord jump or empty
	SessionID    string // optional Grok/session id
	// GitHub map (optional). Empty Login = unmapped.
	GitHubLogin string
	GitHubName  string
	GitHubEmail string // optional; empty → noreply derived when login set
}

// attributionFooter is a thin wrapper for tests / call sites that only have Discord fields.
func attributionFooter(prompter, prompterID, threadURL string) string {
	return BuildAttributionBlock(AttributionInput{
		PrompterName: prompter,
		PrompterID:   prompterID,
		ThreadURL:    threadURL,
	})
}

// BuildAttributionBlock is the Tier A ship contract block: PR/commit footer + trailers.
// Host remains the pusher; this only instructs the model what text to include.
// Human-visible attribution uses display name + optional GitHub @login only —
// never Discord snowflakes or Discord thread jump links.
func BuildAttributionBlock(in AttributionInput) string {
	var b strings.Builder
	b.WriteString("\nAttribution (required when you ship — PR body footer and commit message trailers):\n")
	b.WriteString("The host bot still pushes and opens the PR; you must still record who asked.\n")
	b.WriteString("Do not put Discord user IDs or Discord thread URLs in the PR body or commit trailers.\n")

	// Human-readable summary for the model (no Discord id / thread link).
	if name := strings.TrimSpace(in.PrompterName); name != "" {
		b.WriteString("- Prompter: ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	login := strings.TrimPrefix(strings.TrimSpace(in.GitHubLogin), "@")
	if login != "" {
		b.WriteString("- GitHub: @")
		b.WriteString(login)
		b.WriteString("\n")
	}
	if in.SessionID != "" {
		b.WriteString("- Session: ")
		b.WriteString(in.SessionID)
		b.WriteString("\n")
	}

	// Required copy-paste footer for PR body (and direct-ship commit messages).
	b.WriteString("\nAppend this exact footer block to the PR body (and to the commit message body for direct-to-primary ship):\n")
	b.WriteString("```\n")
	b.WriteString(AttributionPRFooterText(in))
	b.WriteString("```\n")

	// Commit trailers.
	trailers := AttributionCommitTrailers(in)
	if trailers != "" {
		b.WriteString("\nOn every commit that ships this work, include these git trailers (blank line before trailers):\n")
		b.WriteString("```\n")
		b.WriteString(trailers)
		b.WriteString("```\n")
	}
	if login != "" {
		name, email := AttributionAuthorFields(in)
		if name != "" && email != "" {
			b.WriteString("Optional: if you set git author for this commit, use name \"")
			b.WriteString(name)
			b.WriteString("\" and email \"")
			b.WriteString(email)
			b.WriteString("\" (committer may remain the host bot).\n")
		}
	}
	return b.String()
}

// AttributionPRFooterText is the durable PR-body / direct-ship message footer (no fences).
// Omits Discord snowflakes and Discord thread URLs.
func AttributionPRFooterText(in AttributionInput) string {
	var lines []string
	lines = append(lines, "---")
	lines = append(lines, "Requested via Grok Work")
	if name := strings.TrimSpace(in.PrompterName); name != "" {
		lines = append(lines, "Prompter: "+name)
	}
	login := strings.TrimPrefix(strings.TrimSpace(in.GitHubLogin), "@")
	if login != "" {
		lines = append(lines, "GitHub: @"+login)
	}
	if in.SessionID != "" {
		lines = append(lines, "Session: "+in.SessionID)
	}
	return strings.Join(lines, "\n") + "\n"
}

// AttributionCommitTrailers returns Co-authored-by (when mapped) + Prompter name.
// No Discord id and no Discord thread URL.
func AttributionCommitTrailers(in AttributionInput) string {
	var lines []string
	login := strings.TrimPrefix(strings.TrimSpace(in.GitHubLogin), "@")
	if login != "" {
		name, email := AttributionAuthorFields(in)
		if name != "" && email != "" {
			lines = append(lines, "Co-authored-by: "+name+" <"+email+">")
		}
	}
	if name := strings.TrimSpace(in.PrompterName); name != "" {
		lines = append(lines, "Prompter: "+name)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// AttributionAuthorFields returns suggested GIT_AUTHOR name/email for a mapped identity.
// Unmapped → empty strings.
func AttributionAuthorFields(in AttributionInput) (name, email string) {
	login := strings.TrimPrefix(strings.TrimSpace(in.GitHubLogin), "@")
	if login == "" {
		return "", ""
	}
	name = strings.TrimSpace(in.GitHubName)
	if name == "" {
		name = login
	}
	email = strings.TrimSpace(in.GitHubEmail)
	if email == "" {
		email = config.NoreplyGitHubEmail(in.PrompterID, login)
	}
	return name, email
}

// OnBehalfOfCommentBody prefixes a host-bot GitHub comment when the acting Discord
// user is mapped to a GitHub login. Unmapped actors and empty/whitespace-only bodies
// are returned unchanged — no invented @login. Does not include Discord snowflakes.
//
// Example (mapped):
//
//	On behalf of @alice (Alice):
//
//	please merge when green
func OnBehalfOfCommentBody(discordID, displayName, githubLogin, body string) string {
	if strings.TrimSpace(body) == "" {
		return body
	}
	login := strings.TrimPrefix(strings.TrimSpace(githubLogin), "@")
	if login == "" {
		return body
	}
	_ = discordID // kept in signature for call-site compatibility; never written into body
	displayName = strings.TrimSpace(displayName)
	var who strings.Builder
	who.WriteString("On behalf of @")
	who.WriteString(login)
	if displayName != "" {
		who.WriteString(" (")
		who.WriteString(displayName)
		who.WriteString(")")
	}
	who.WriteString(":\n\n")
	who.WriteString(body)
	return who.String()
}

// intentPreview truncates a prompt for queue display (~80 runes).
func intentPreview(prompt string, maxRunes int) string {
	prompt = strings.TrimSpace(prompt)
	if maxRunes <= 0 {
		maxRunes = 80
	}
	r := []rune(prompt)
	if len(r) <= maxRunes {
		return prompt
	}
	return string(r[:maxRunes-1]) + "…"
}
