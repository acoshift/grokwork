package config

import (
	"fmt"
	"strings"
)

// LookupGitHubIdentity returns the Tier A map entry for a Discord user id.
// Missing or empty login → false (unmapped).
func (c *Config) LookupGitHubIdentity(discordUserID string) (GitHubIdentity, bool) {
	if c == nil {
		return GitHubIdentity{}, false
	}
	discordUserID = strings.TrimSpace(discordUserID)
	if discordUserID == "" {
		return GitHubIdentity{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.DiscordUserGitHub == nil {
		return GitHubIdentity{}, false
	}
	id, ok := c.DiscordUserGitHub[discordUserID]
	if !ok {
		return GitHubIdentity{}, false
	}
	id.Login = strings.TrimPrefix(strings.TrimSpace(id.Login), "@")
	id.Name = strings.TrimSpace(id.Name)
	id.Email = strings.TrimSpace(id.Email)
	if id.Login == "" {
		return GitHubIdentity{}, false
	}
	return id, true
}

// SetGitHubIdentity maps discordUserID → identity and persists.
// Login is required (bare, without @).
func (c *Config) SetGitHubIdentity(discordUserID string, id GitHubIdentity) error {
	discordUserID = strings.TrimSpace(discordUserID)
	if discordUserID == "" {
		return fmt.Errorf("discord user id is required")
	}
	id.Login = strings.TrimPrefix(strings.TrimSpace(id.Login), "@")
	id.Name = strings.TrimSpace(id.Name)
	id.Email = strings.TrimSpace(id.Email)
	if id.Login == "" {
		return fmt.Errorf("github login is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DiscordUserGitHub == nil {
		c.DiscordUserGitHub = map[string]GitHubIdentity{}
	}
	c.DiscordUserGitHub[discordUserID] = id
	return c.saveLocked()
}

// RemoveGitHubIdentity clears a Discord user mapping and persists.
func (c *Config) RemoveGitHubIdentity(discordUserID string) error {
	discordUserID = strings.TrimSpace(discordUserID)
	if discordUserID == "" {
		return fmt.Errorf("discord user id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.DiscordUserGitHub == nil {
		return nil
	}
	delete(c.DiscordUserGitHub, discordUserID)
	if len(c.DiscordUserGitHub) == 0 {
		c.DiscordUserGitHub = nil
	}
	return c.saveLocked()
}

// NoreplyGitHubEmail builds the users.noreply.github.com address for Co-authored-by.
// Prefer id+login when Discord id is numeric; else login@users.noreply.github.com.
func NoreplyGitHubEmail(discordUserID, login string) string {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if login == "" {
		return ""
	}
	discordUserID = strings.TrimSpace(discordUserID)
	if discordUserID != "" {
		return discordUserID + "+" + login + "@users.noreply.github.com"
	}
	return login + "@users.noreply.github.com"
}

// EffectiveGitHubEmail returns configured email or noreply default.
func (id GitHubIdentity) EffectiveEmail(discordUserID string) string {
	if e := strings.TrimSpace(id.Email); e != "" {
		return e
	}
	return NoreplyGitHubEmail(discordUserID, id.Login)
}

// EffectiveName returns configured name or GitHub login.
func (id GitHubIdentity) EffectiveName() string {
	if n := strings.TrimSpace(id.Name); n != "" {
		return n
	}
	return strings.TrimPrefix(strings.TrimSpace(id.Login), "@")
}

func cloneGitHubIdentityMap(m map[string]GitHubIdentity) map[string]GitHubIdentity {
	if m == nil {
		return nil
	}
	out := make(map[string]GitHubIdentity, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
