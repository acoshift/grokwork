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
	KindTask
)

type Parsed struct {
	Kind   Kind
	Prompt string
}

var mentionRE = regexp.MustCompile(`<@!?\d+>`)

// ParseMessage extracts a task prompt from a Discord message body.
// Special characters in the prompt (#, ?, &, URLs, fragments) are preserved.
func ParseMessage(content, botUserID string) Parsed {
	text := normalizeUserPrompt(stripBotMention(content, botUserID))

	if text == "" {
		return Parsed{Kind: KindEmpty}
	}

	switch strings.ToLower(text) {
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
	}

	return Parsed{Kind: KindTask, Prompt: text}
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
		"**Grok Discord bridge** — runs Grok Build on this machine against local code.",
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
		"• `/reset` — forget this thread's session and remove its worktree",
		"• `/status` — show this thread's session, PR, and queue depth if busy",
		"• `/cancel` — stop the current run (queued follow-ups still run)",
		"• `/help` — this message",
	}, "\n")
}
