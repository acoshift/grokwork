package bot

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

type Kind int

const (
	KindEmpty Kind = iota
	KindHelp
	KindProjects
	KindReset
	KindStatus
	KindCancel
	KindFixCI
	KindClaim
	KindHandOff
	KindBrief
	KindLabel
	KindBoard
	KindLink
	KindReview
	KindQueue
	KindDequeue
	KindCancelMine
	KindStartInvestigate
	KindStartFix
	KindStartExplain
	KindTask
)

type Parsed struct {
	Kind   Kind
	Prompt string
	// Arg is optional argument text (queue index, start body, etc.).
	Arg string
}

var mentionRE = regexp.MustCompile(`<@!?\d+>`)

// ParseMessage extracts a task prompt from a Discord message body.
// Special characters in the prompt (#, ?, &, URLs, fragments) are preserved.
func ParseMessage(content, botUserID string) Parsed {
	text := normalizeUserPrompt(stripBotMention(content, botUserID))

	if text == "" {
		return Parsed{Kind: KindEmpty}
	}

	lower := strings.ToLower(text)
	switch lower {
	case "/help", "help":
		return Parsed{Kind: KindHelp}
	case "/projects", "projects":
		return Parsed{Kind: KindProjects}
	case "/reset", "reset":
		return Parsed{Kind: KindReset}
	case "/status", "status":
		return Parsed{Kind: KindStatus}
	case "/cancel", "cancel", "/stop", "stop":
		return Parsed{Kind: KindCancel}
	case "/fix-ci", "fix-ci", "/fixci", "fixci":
		return Parsed{Kind: KindFixCI}
	case "/claim", "claim":
		return Parsed{Kind: KindClaim}
	case "/brief", "brief":
		return Parsed{Kind: KindBrief, Prompt: text}
	case "/label", "label":
		return Parsed{Kind: KindLabel, Prompt: text}
	case "/board", "board":
		return Parsed{Kind: KindBoard, Prompt: text}
	case "/link", "link":
		return Parsed{Kind: KindLink, Prompt: text}
	case "/unlink", "unlink":
		return Parsed{Kind: KindLink, Prompt: text}
	case "/review", "review":
		return Parsed{Kind: KindReview, Prompt: text}
	case "/queue", "queue":
		return Parsed{Kind: KindQueue, Prompt: text}
	case "/cancel-mine", "cancel-mine", "/cancelmine", "cancelmine":
		return Parsed{Kind: KindCancelMine, Prompt: text}
	}

	if isStartCommand(lower, text) {
		return parseStartCommand(text)
	}
	if isDequeueCommand(lower) {
		return Parsed{Kind: KindDequeue, Prompt: text, Arg: strings.TrimSpace(text[len("/dequeue"):])}
	}
	if isHandOffCommand(lower) {
		return Parsed{Kind: KindHandOff, Prompt: text}
	}
	if isBriefCommand(lower) {
		return Parsed{Kind: KindBrief, Prompt: text}
	}
	if isLabelCommand(lower) {
		return Parsed{Kind: KindLabel, Prompt: text}
	}
	if isBoardCommand(lower) {
		return Parsed{Kind: KindBoard, Prompt: text}
	}
	if isLinkCommand(lower) {
		return Parsed{Kind: KindLink, Prompt: text}
	}
	if isReviewCommand(lower) {
		return Parsed{Kind: KindReview, Prompt: text}
	}

	return Parsed{Kind: KindTask, Prompt: text}
}

func isHandOffCommand(lower string) bool {
	switch lower {
	case "/hand-off", "/handoff", "hand-off", "handoff":
		return true
	}
	// Args only with leading slash so free-form "hand-off notes…" stays a task.
	return strings.HasPrefix(lower, "/hand-off ") || strings.HasPrefix(lower, "/handoff ")
}

func isBriefCommand(lower string) bool {
	// "/brief …" always a command. Bare "brief goal …" / "brief set goal …" too.
	// Free-form "brief notes for the team" stays a task.
	if strings.HasPrefix(lower, "/brief ") {
		return true
	}
	return strings.HasPrefix(lower, "brief goal ") || strings.HasPrefix(lower, "brief set goal ")
}

func isLabelCommand(lower string) bool {
	// "/label …" always a command. Bare "label" alone is already handled; bare
	// "label blocked" would steal free-form tasks — require leading slash for args.
	return strings.HasPrefix(lower, "/label ")
}

func isBoardCommand(lower string) bool {
	return strings.HasPrefix(lower, "/board ")
}

func isReviewCommand(lower string) bool {
	// "/review @user …" only — bare "review the flaky test" stays a task.
	return strings.HasPrefix(lower, "/review ")
}

func isLinkCommand(lower string) bool {
	// "/link …" and "/unlink …" always commands. Bare "link the docs" stays a task.
	return strings.HasPrefix(lower, "/link ") || strings.HasPrefix(lower, "/unlink ")
}

func isDequeueCommand(lower string) bool {
	return strings.HasPrefix(lower, "/dequeue ") || lower == "/dequeue"
}

func isStartCommand(lower, text string) bool {
	return strings.HasPrefix(lower, "/start ") || lower == "/start" ||
		strings.HasPrefix(lower, "/investigate ") || lower == "/investigate"
}

func parseStartCommand(text string) Parsed {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return Parsed{Kind: KindTask, Prompt: text}
	}
	cmd := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	switch cmd {
	case "/start":
		if rest == "" {
			return Parsed{Kind: KindHelp, Prompt: text}
		}
		subFields := strings.Fields(rest)
		sub := strings.ToLower(subFields[0])
		body := ""
		if len(subFields) > 1 {
			body = strings.TrimSpace(rest[len(subFields[0]):])
		}
		switch sub {
		case "investigate":
			return Parsed{Kind: KindStartInvestigate, Prompt: body, Arg: sub}
		case "fix":
			return Parsed{Kind: KindStartFix, Prompt: body, Arg: sub}
		case "explain":
			return Parsed{Kind: KindStartExplain, Prompt: body, Arg: sub}
		default:
			// /start <freeform as investigate-or-fix via default>
			return Parsed{Kind: KindStartFix, Prompt: rest, Arg: "fix"}
		}
	case "/investigate":
		return Parsed{Kind: KindStartInvestigate, Prompt: rest, Arg: "investigate"}
	default:
		return Parsed{Kind: KindTask, Prompt: text}
	}
}

func stripBotMention(content, botUserID string) string {
	if botUserID != "" {
		re := regexp.MustCompile(fmt.Sprintf(`<@!?%s>`, regexp.QuoteMeta(botUserID)))
		return re.ReplaceAllString(content, " ")
	}
	return mentionRE.ReplaceAllString(content, " ")
}

// normalizeUserPrompt trims and collapses whitespace without altering #, ?, &,
// or other non-space characters that appear in issue refs and URLs.
func normalizeUserPrompt(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == 0:
			return -1
		case r == '\u00a0': // NBSP from some clients
			return ' '
		case unicode.IsSpace(r):
			// Fields-style: any unicode space becomes a separator later.
			return ' '
		default:
			return r
		}
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// messagePromptText builds prompt text from a Discord message, including embed
// URLs/titles when content is empty or when Discord only surface-linked a URL.
func messagePromptText(m *discordgo.Message) string {
	if m == nil {
		return ""
	}
	var parts []string
	if c := strings.TrimSpace(m.Content); c != "" {
		// Discord clients often wrap paste-links as <https://...> to suppress embeds.
		parts = append(parts, unwrapDiscordLinks(c))
	}
	for _, e := range m.Embeds {
		if e == nil {
			continue
		}
		if u := strings.TrimSpace(e.URL); u != "" {
			parts = append(parts, u)
		}
		if t := strings.TrimSpace(e.Title); t != "" {
			parts = append(parts, t)
		}
		if d := strings.TrimSpace(e.Description); d != "" {
			parts = append(parts, unwrapDiscordLinks(d))
		}
	}
	return normalizeUserPrompt(strings.Join(parts, "\n"))
}

func HelpText() string {
	return strings.Join([]string{
		"**Grok Work bridge** — runs Grok Build on this machine against local code.",
		"",
		"**Usage**",
		"• `@Grok <task>` — run against this channel's configured project",
		"• `@Grok <follow-up>` in the same thread — resume session",
		"• Follow-ups while a run is active are queued (up to 5) and run in order",
		"• Attach logs/screenshots/patches with your message — files are downloaded for Grok to read",
		"• Or post a file, then **reply** with `@Grok <task>` — Grok reads the referenced message too",
		"• Ask Grok to build/export artifacts (APK, Excel, …) — files **inside the thread worktree** can be uploaded back to Discord",
		"",
		"Project is fixed per Discord channel (admin `channels` config). Users cannot switch projects.",
		"Each thread uses an isolated git worktree (when the project is a git repo). `/reset` removes it.",
		"Code changes are pushed and opened as a pull request (not left as local-only commits).",
		"Discord file uploads only allow paths under that worktree (not the main checkout).",
		"",
		"**Commands** (mention the bot first)",
		"• `/projects` — show this channel's project",
		"• `/status` — show this thread's owner, session, label, issue, PR, and queue depth if busy",
		"• `/brief` — pin/update the continuity card (goal, done/left, branch, issue, PR, files)",
		"• `/brief goal <text>` — set the sticky goal, then refresh the card",
		"• `/label` — show lifecycle label; `/label <open|in_progress|blocked|needs_review|done|abandoned>` sets manual; `/label auto` re-enables auto",
		"• `/board [running|queued|waiting|stale|label|all]` — team activity board for this channel's project (running, queued, waiting on human, stale)",
		"• `/link #N` or `/link ENG-123` — bind GitHub/Linear tickets (Linear only when enabled per project); `/link fix …` uses `Fixes`; `/unlink`; `/link clear`",
		"• `/review @user [optional #N|PR URL]` — request a team review (Discord identity; shows on web My reviews)",
		"• `/claim` — take ownership of this thread (anyone on the allowlist)",
		"• `/hand-off @user` — transfer ownership and post a short hand-off card",
		"• `/reset` — forget this thread's session and remove its worktree (owner/mod)",
		"• `/fix-ci` — fetch failing CI checks and queue a minimal fix on this PR branch",
		"• `/cancel` — stop the current run (owner/mod; queued follow-ups still run)",
		"• `/queue` — list queued follow-ups (author + intent)",
		"• `/dequeue N` — remove queue item N (1-based; owner/mod or your own)",
		"• `/cancel-mine` — remove your queued items",
		"• `/start investigate|fix|explain <task>` — set session mode and run",
		"• `/investigate <task>` — read-only investigate (no PR / no direct ship)",
		"• `/help` — this message",
		"",
		"**Run action bar** — buttons on the live status / done message and `/status`:",
		"Cancel · Continue (modal) · Reset (confirm) · History (admin UI path)",
		"",
		"Anyone may queue tasks (soft open). Cancel/reset: thread owner, co-owners, or Discord mods (Manage Messages / Manage Threads / Admin).",
		"Investigate mode never opens PRs or ships to primary. SafeTeamMode maps unmapped users to investigator.",
	}, "\n")
}
