package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// Discord permission bits used by this bot (Developer Portal → Bot → invite).
// View Channel | Send Messages | Manage Messages | Pin Messages (brief card) |
// Attach Files | Read Message History | Create Public Threads | Send Messages in Threads
//
// PIN_MESSAGES (1<<51) is required to pin/unpin. Discord split it out of
// MANAGE_MESSAGES; Manage Messages alone is no longer enough for pins.
const BotInvitePermissions int64 = (1 << 10) | // VIEW_CHANNEL
	(1 << 11) | // SEND_MESSAGES
	(1 << 13) | // MANAGE_MESSAGES
	(1 << 15) | // ATTACH_FILES
	(1 << 16) | // READ_MESSAGE_HISTORY
	(1 << 35) | // CREATE_PUBLIC_THREADS
	(1 << 38) | // SEND_MESSAGES_IN_THREADS
	(1 << 51) // PIN_MESSAGES — pin continuity brief card

// BotInviteScopes is the OAuth2 scope for the install URL.
const BotInviteScopes = "bot"

// ClientID returns the Discord application/client ID.
// Prefer discordClientId from config; otherwise decode it from the bot token.
func (c *Config) ClientID() (string, error) {
	c.mu.RLock()
	explicit := strings.TrimSpace(c.DiscordClientID)
	token := c.DiscordToken
	c.mu.RUnlock()
	if explicit != "" {
		return explicit, nil
	}
	return ClientIDFromToken(token)
}

// InviteURL builds the Discord bot install/authorize URL for this app.
func (c *Config) InviteURL() (string, error) {
	id, err := c.ClientID()
	if err != nil {
		return "", err
	}
	return BuildInviteURL(id, BotInvitePermissions, BotInviteScopes), nil
}

// BuildInviteURL constructs a Discord OAuth2 bot authorize URL.
func BuildInviteURL(clientID string, permissions int64, scope string) string {
	clientID = strings.TrimSpace(clientID)
	if scope == "" {
		scope = BotInviteScopes
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("permissions", fmt.Sprintf("%d", permissions))
	q.Set("scope", scope)
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// ClientIDFromToken extracts the bot user/application ID from a Discord bot token.
// Token format: base64(userId).timestamp.hmac
func ClientIDFromToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "Bot ")
	if token == "" || token == "YOUR_BOT_TOKEN" {
		return "", fmt.Errorf("discord token not set")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid bot token format")
	}
	s := parts[0]
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Some tokens use raw encoding without padding already handled.
		raw, err = base64.RawStdEncoding.DecodeString(parts[0])
		if err != nil {
			return "", fmt.Errorf("decode client id from token: %w", err)
		}
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("empty client id in token")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("decoded client id is not a snowflake")
		}
	}
	return id, nil
}
