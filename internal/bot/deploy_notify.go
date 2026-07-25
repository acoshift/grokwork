package bot

import (
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/inbox"
)

// SendProjectMessage posts to a project's Discord channel.
//
// Exists for the deploy engine, which must not depend on Discord itself. The
// error on a project with no channel is meaningful: it is the caller's signal
// to fall back to the inbox rather than drop the notice.
func (b *Bot) SendProjectMessage(project, content string) error {
	if b == nil {
		return fmt.Errorf("bot: nil")
	}
	ch, err := b.cfg.PreferDiscordChannel(project)
	if err != nil {
		return err
	}
	s := b.Discord()
	if s == nil {
		return fmt.Errorf("bot: no discord session")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("bot: empty message")
	}
	// Same sanitizer the rest of the bot uses: local paths must never reach
	// Discord even if a caller assembles them by accident.
	_, err = s.ChannelMessageSend(ch, sanitizeDiscordContent(clampDiscord(content)))
	return err
}

// AppendInbox adds one item to an actor's feed.
func (b *Bot) AppendInbox(actorID, kind, subject, body, url, project string) error {
	if b == nil || b.inbox == nil {
		return fmt.Errorf("bot: no inbox")
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return fmt.Errorf("bot: empty actor")
	}
	_, err := b.inbox.Append(actorID, inbox.Item{
		Kind: kind, Subject: subject, Body: body, URL: url, Project: project,
	})
	return err
}
