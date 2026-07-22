package bot

import (
	"strings"

	"github.com/acoshift/grokwork/internal/config"
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
	Mode             string
	Phase            string
	RunKind          string
	AllowPR          bool
	AllowDirectShip  bool
	Yolo             bool
	Tools            *string // nil unrestricted; non-nil → allowlist / tools-off
	NoSubagents      bool
	IncludeGHToken   bool
	PrefixKind       string // "remote" | "investigate" | "explain" | "none"
	RefreshPR        bool
	RefreshPRWarnOnly bool
	PostCompletion   string // "eng" | "dossier" | "none"
	RefreshBrief     bool
	AllowUpload      bool
	AllowDirectIntegrate bool
	DirtyTreeWarn    bool
	Coerced          bool // StartSessions without GithubWrites coerced to investigate
}

// PolicyInput is the pure decision input for BuildRunPolicy.
type PolicyInput struct {
	SessionMode   string // Entry.Mode or empty
	SessionPhase  string // Wave 3; empty in Wave 1
	ShipMode      string // sessionstore.ShipModePR | ShipModeDirect | ""
	Caps          config.Capabilities
	ConfigYolo    bool
	// RequestedMode from /start or freeform inherit; empty → session or fix default.
	RequestedMode string
	// RequestedRunKind optional explicit kind (fix_ci, address).
	RequestedRunKind string
	// ForceInvestigate forces investigate policy (e.g. /start investigate).
	ForceInvestigate bool
	// SafeTeamMode on project (affects nothing here; caps already resolved).
	InvestigateTools string // project override; empty → default read allowlist
}

// DefaultInvestigateTools is the best-effort Wave 1 tools allowlist (K21).
// Host probe may refine later; fail-closed tools-off is "" pointer rewrite in grokrun.
const DefaultInvestigateTools = "read_file,grep"

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

	// Case closed: reject runs (PrefixKind none) — handler should not enqueue.
	if mode == ModeCase && phase == sessionstore.PhaseClosed {
		return RunPolicy{
			Mode: ModeCase, Phase: phase, RunKind: rk,
			PrefixKind: "none", PostCompletion: "none",
			Coerced: coerced,
		}
	}

	// Case non-ship phases + investigate/explain: non-shipping (K27).
	caseNonShip := mode == ModeCase && isCaseNonShipPhase(phase)
	if mode == ModeInvestigate || mode == ModeExplain || caseNonShip {
		if mode == ModeCase {
			// Keep Mode=case; phase stays; run kind investigate unless explicit
			if rk == RunKindFix {
				rk = RunKindInvestigate
			}
		}
		tools := in.InvestigateTools
		if tools == "" {
			tools = DefaultInvestigateTools
		}
		toolsCopy := tools
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
		}
		if mode == ModeExplain || phase == sessionstore.PhaseAnswered {
			pol.PrefixKind = "explain"
			pol.PostCompletion = "none"
			empty := ""
			pol.Tools = &empty // tools-off rewrite
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

// investigatePromptPrefix is the non-shipping contract (no PR, no direct ship).
func investigatePromptPrefix(branch string) string {
	lines := []string{
		"You are investigating code on a shared workflow unit (Discord thread and/or web session).",
		"Mode: INVESTIGATE (read-only intent). Do NOT commit, push, open a pull request, or modify the remote.",
		"Do NOT run `gh pr create`, do NOT push to main/master, and do NOT merge.",
		"Explain findings in plain language. Prefer reading code and summarizing root cause.",
		"If you need a code change, say so and stop — a human will start a fix run.",
		"Do not claim the issue is fixed unless you only confirmed existing behavior.",
		"",
		"Filesystem scope: stay inside this unit's cwd/worktree and the project repo.",
		"Do NOT scan the user's home directory or protected folders.",
		"",
	}
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

// attributionFooter appends Tier A attribution lines for ship runs.
func attributionFooter(prompter, prompterID, threadURL string) string {
	var b strings.Builder
	b.WriteString("\nAttribution (required in PR body / commit message when you ship):\n")
	if prompter != "" {
		b.WriteString("- Prompter: ")
		b.WriteString(prompter)
		if prompterID != "" {
			b.WriteString(" (Discord ")
			b.WriteString(prompterID)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	if threadURL != "" {
		b.WriteString("- Thread: ")
		b.WriteString(threadURL)
		b.WriteString("\n")
	}
	b.WriteString("- Include a trailer line: Prompter-Discord: <id or name>; Thread: <url>\n")
	b.WriteString("")
	return b.String()
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
