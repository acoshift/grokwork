package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// WebRole is the private web UI authorization level for a Discord user.
type WebRole string

const (
	WebRoleNone   WebRole = ""
	WebRoleViewer WebRole = "viewer"
	WebRoleMember WebRole = "member"
	WebRoleAdmin  WebRole = "admin"
)

// WebAuthFeatures are request-time feature gates for future write routes.
// PR1 only gates existing config/worktree mutations behind auth+admin when Enabled.
type WebAuthFeatures struct {
	GitHubWrites  bool `json:"githubWrites,omitempty"`
	Merge         bool `json:"merge,omitempty"`
	StartSessions bool `json:"startSessions,omitempty"`
}

// WebAuthConfig is optional private-web authentication (Discord OAuth).
// When Enabled is false or WebAuth is nil, the web UI stays open LAN mode (legacy).
type WebAuthConfig struct {
	Enabled          bool           `json:"enabled"`
	SessionSecret    string         `json:"sessionSecret,omitempty"`
	AdminDiscordIDs  []string       `json:"adminDiscordIds,omitempty"`
	MemberDiscordIDs []string       `json:"memberDiscordIds,omitempty"`
	ViewerDiscordIDs []string       `json:"viewerDiscordIds,omitempty"`
	Features         WebAuthFeatures `json:"features,omitempty"`
}

// WebAuthEnabled reports whether Discord OAuth web auth is turned on.
func (c *Config) WebAuthEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.WebAuth != nil && c.WebAuth.Enabled
}

// DiscordClientSecretValue returns the OAuth client secret (config or env).
func (c *Config) DiscordClientSecretValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	secret := strings.TrimSpace(c.DiscordClientSecret)
	c.mu.RUnlock()
	if secret != "" {
		return secret
	}
	// Plain Discord name first, then product env.
	return firstEnv(
		"DISCORD_CLIENT_SECRET",
		"GROK_WORK_DISCORD_CLIENT_SECRET",
	)
}

// WebPublicBaseURLValue returns the public base URL for OAuth redirect_uri construction.
func (c *Config) WebPublicBaseURLValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	base := strings.TrimSpace(c.WebPublicBaseURL)
	c.mu.RUnlock()
	if base != "" {
		return strings.TrimRight(base, "/")
	}
	if v := EnvWork("PUBLIC_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
}

// SessionSecretValue returns the web session HMAC/store secret (config or env).
func (c *Config) SessionSecretValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	var secret string
	if c.WebAuth != nil {
		secret = strings.TrimSpace(c.WebAuth.SessionSecret)
	}
	c.mu.RUnlock()
	if secret != "" {
		return secret
	}
	return EnvWork("SESSION_SECRET")
}

// WebAuthAdminIDs returns a copy of admin Discord user IDs (after bootstrap merge).
func (c *Config) WebAuthAdminIDs() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.WebAuth == nil {
		return nil
	}
	return slices.Clone(c.WebAuth.AdminDiscordIDs)
}

// FeatureGitHubWrites / FeatureMerge / FeatureStartSessions are request-time gates.
// Always false when webAuth is disabled (fail-closed for open LAN).
func (c *Config) FeatureGitHubWrites() bool {
	return c.featureFlag(func(f WebAuthFeatures) bool { return f.GitHubWrites })
}
func (c *Config) FeatureMerge() bool {
	return c.featureFlag(func(f WebAuthFeatures) bool { return f.Merge })
}
func (c *Config) FeatureStartSessions() bool {
	return c.featureFlag(func(f WebAuthFeatures) bool { return f.StartSessions })
}

func (c *Config) featureFlag(fn func(WebAuthFeatures) bool) bool {
	if c == nil || !c.WebAuthEnabled() {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.WebAuth == nil {
		return false
	}
	return fn(c.WebAuth.Features)
}

// WebMergeMethodValue returns squash|merge|rebase (default squash).
func (c *Config) WebMergeMethodValue() string {
	if c == nil {
		return "squash"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.webMergeMethodLocked()
}

func (c *Config) webMergeMethodLocked() string {
	switch strings.ToLower(strings.TrimSpace(c.WebMergeMethod)) {
	case "merge", "rebase", "squash":
		return strings.ToLower(strings.TrimSpace(c.WebMergeMethod))
	default:
		return "squash"
	}
}

// AnyWriteFeatureEnabled reports githubWrites|merge|startSessions when auth is on.
func (c *Config) AnyWriteFeatureEnabled() bool {
	return c.FeatureGitHubWrites() || c.FeatureMerge() || c.FeatureStartSessions()
}

// RoleResolveInput is the pure input for ResolveWebRole (unit-testable).
type RoleResolveInput struct {
	AdminIDs       []string
	MemberIDs      []string
	ViewerIDs      []string
	ProjectUserIDs []string // union of projects.*.allowedUserIds → member
	ProjectUserSet map[string]struct{}
}

// ResolveWebRole maps a Discord user id to a web role.
// Order: admin list → member list → viewer list → any project user allowlist → deny.
// Returns (role, ok). ok=false means login must be denied.
func ResolveWebRole(discordUserID string, in RoleResolveInput) (WebRole, bool) {
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return WebRoleNone, false
	}
	if containsID(in.AdminIDs, id) {
		return WebRoleAdmin, true
	}
	if containsID(in.MemberIDs, id) {
		return WebRoleMember, true
	}
	if containsID(in.ViewerIDs, id) {
		return WebRoleViewer, true
	}
	if in.ProjectUserSet != nil {
		if _, ok := in.ProjectUserSet[id]; ok {
			return WebRoleMember, true
		}
	} else if containsID(in.ProjectUserIDs, id) {
		return WebRoleMember, true
	}
	return WebRoleNone, false
}

// ResolveWebRoleForConfig uses live webAuth lists + project membership.
func (c *Config) ResolveWebRoleForConfig(discordUserID string) (WebRole, bool) {
	if c == nil {
		return WebRoleNone, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	in := RoleResolveInput{}
	if c.WebAuth != nil {
		in.AdminIDs = c.WebAuth.AdminDiscordIDs
		in.MemberIDs = c.WebAuth.MemberDiscordIDs
		in.ViewerIDs = c.WebAuth.ViewerDiscordIDs
	}
	set := map[string]struct{}{}
	for _, pc := range c.Projects {
		for _, uid := range pc.AllowedUserIDs {
			uid = strings.TrimSpace(uid)
			if uid != "" {
				set[uid] = struct{}{}
			}
		}
	}
	in.ProjectUserSet = set
	return ResolveWebRole(discordUserID, in)
}

// RoleAtLeast reports whether have is at least want (admin > member > viewer).
func RoleAtLeast(have, want WebRole) bool {
	return roleRank(have) >= roleRank(want)
}

func roleRank(r WebRole) int {
	switch r {
	case WebRoleAdmin:
		return 3
	case WebRoleMember:
		return 2
	case WebRoleViewer:
		return 1
	default:
		return 0
	}
}

// ValidateWebAuth checks OAuth prerequisites when web auth is enabled.
// Call after bootstrap admin merge. Nil/disabled is always ok.
func (c *Config) ValidateWebAuth() error {
	if c == nil || !c.WebAuthEnabled() {
		return nil
	}
	var missing []string
	// sessionSecret is optional: sessions are opaque server-side IDs, not signed cookies.
	if strings.TrimSpace(c.EffectiveClientID()) == "" {
		missing = append(missing, "discordClientId (or decodable bot token)")
	}
	if c.DiscordClientSecretValue() == "" {
		missing = append(missing, "discordClientSecret (or DISCORD_CLIENT_SECRET)")
	}
	if c.WebPublicBaseURLValue() == "" {
		missing = append(missing, "webPublicBaseURL (or GROK_WORK_PUBLIC_BASE_URL)")
	}
	admins := c.WebAuthAdminIDs()
	if len(admins) == 0 {
		missing = append(missing, "webAuth.adminDiscordIds (or GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("webAuth.enabled requires: %s", strings.Join(missing, "; "))
	}
	return nil
}

// EffectiveClientID returns configured client id or decodes from bot token (best effort).
func (c *Config) EffectiveClientID() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	explicit := strings.TrimSpace(c.DiscordClientID)
	token := c.DiscordToken
	c.mu.RUnlock()
	if explicit != "" {
		return explicit
	}
	id, err := ClientIDFromToken(token)
	if err != nil {
		return ""
	}
	return id
}

// applyWebAuthBootstrap merges bootstrap env into admin list and env secrets when needed.
// Must be called under no lock or with ownership of c during Load.
func (c *Config) applyWebAuthBootstrap() {
	if c.WebAuth == nil {
		c.WebAuth = &WebAuthConfig{}
	}
	// Bootstrap admin when list empty.
	if len(c.WebAuth.AdminDiscordIDs) == 0 {
		if v := EnvWork("BOOTSTRAP_ADMIN_DISCORD_ID"); v != "" {
			c.WebAuth.AdminDiscordIDs = []string{v}
			fmt.Fprintf(os.Stderr, "[info] webAuth: bootstrapped admin Discord id from GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID\n")
		}
	}
	// Normalize ID slices (trim empties).
	c.WebAuth.AdminDiscordIDs = cleanIDList(c.WebAuth.AdminDiscordIDs)
	c.WebAuth.MemberDiscordIDs = cleanIDList(c.WebAuth.MemberDiscordIDs)
	c.WebAuth.ViewerDiscordIDs = cleanIDList(c.WebAuth.ViewerDiscordIDs)
}

func cleanIDList(ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if strings.TrimSpace(id) == want {
			return true
		}
	}
	return false
}

func cloneWebAuth(w *WebAuthConfig) *WebAuthConfig {
	if w == nil {
		return nil
	}
	out := *w
	out.AdminDiscordIDs = slices.Clone(w.AdminDiscordIDs)
	out.MemberDiscordIDs = slices.Clone(w.MemberDiscordIDs)
	out.ViewerDiscordIDs = slices.Clone(w.ViewerDiscordIDs)
	out.Features = w.Features
	return &out
}
