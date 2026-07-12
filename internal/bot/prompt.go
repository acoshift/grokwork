package bot

import (
	"fmt"
	"regexp"
	"strings"
)

type Kind int

const (
	KindEmpty Kind = iota
	KindHelp
	KindProjects
	KindReset
	KindStatus
	KindTask
)

type Parsed struct {
	Kind   Kind
	Prompt string
}

var mentionRE = regexp.MustCompile(`<@!?\d+>`)

func ParseMessage(content, botUserID string) Parsed {
	text := content
	if botUserID != "" {
		re := regexp.MustCompile(fmt.Sprintf(`<@!?%s>`, regexp.QuoteMeta(botUserID)))
		text = re.ReplaceAllString(text, " ")
	} else {
		text = mentionRE.ReplaceAllString(text, " ")
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimSpace(text)

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
	}

	return Parsed{Kind: KindTask, Prompt: text}
}

func HelpText() string {
	return strings.Join([]string{
		"**Grok Discord bridge** — runs Grok Build on this machine against local code.",
		"",
		"**Usage**",
		"• `@Grok <task>` — run against this channel's configured project",
		"• `@Grok <follow-up>` in the same thread — resume session",
		"",
		"Project is fixed per Discord channel (admin `channels` config). Users cannot switch projects.",
		"",
		"**Commands** (mention the bot first)",
		"• `/projects` — show this channel's project",
		"• `/reset` — forget this thread's session",
		"• `/status` — show this thread's session",
		"• `/help` — this message",
	}, "\n")
}
