package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPListen          = ":8787"
	DefaultWorktreeIdleTTLDays = 30
)

type Config struct {
	DiscordToken string `json:"discordToken"`
	// DiscordClientID is optional; when empty the client id is decoded from discordToken.
	DiscordClientID      string            `json:"discordClientId,omitempty"`
	AllowedUserIDs       []string          `json:"allowedUserIds"`
	AllowedRoleIDs       []string          `json:"allowedRoleIds"`
	Projects             map[string]string `json:"projects"`
	Channels             map[string]string `json:"channels"` // channel ID → project name
	GrokBin              string            `json:"grokBin"`
	Yolo                 *bool             `json:"yolo"`
	Model                string            `json:"model"`
	MaxTurns             int               `json:"maxTurns"`
	TimeoutMs            int               `json:"timeoutMs"`
	ExtraArgs            []string          `json:"extraArgs"`
	SummarizeThreadTitle *bool             `json:"summarizeThreadTitle"`
	SummarizeTimeoutMs   int               `json:"summarizeTimeoutMs"`
	WorktreeIsolation    *bool             `json:"worktreeIsolation"`
	// WorktreeIdleTTLDays is days of inactivity before pruning thread worktrees.
	// nil/omitted → DefaultWorktreeIdleTTLDays (30). 0 disables idle cleanup.
	WorktreeIdleTTLDays *int `json:"worktreeIdleTTLDays,omitempty"`
	// HTTPListen is the address for the private-network web UI (e.g. ":8787", "0.0.0.0:8787").
	// Empty uses default ":8787". Override with GROK_DISCORD_HTTP_LISTEN.
	HTTPListen string `json:"httpListen,omitempty"`
	// RiskyPathGlobs flags completion-card paths for review (**, * globs).
	// nil/omitted → built-in defaults. Empty slice → no risk highlighting.
	RiskyPathGlobs []string `json:"riskyPathGlobs,omitempty"`
	// AutoFixCI queues a CI fix task when the PR status poller sees failing checks.
	// nil/omitted/false → digest only; user runs @Grok /fix-ci.
	AutoFixCI *bool `json:"autoFixCI,omitempty"`
	// AutoFixCIMax is the max auto-queued fix attempts per thread session (default 2).
	AutoFixCIMax int `json:"autoFixCIMax,omitempty"`

	mu           sync.RWMutex
	AllowedUsers map[string]struct{} `json:"-"`
	AllowedRoles map[string]struct{} `json:"-"`
	DataDir      string              `json:"-"`
	ConfigPath   string              `json:"-"`
}

// ProjectItem is a project row for the config UI.
type ProjectItem struct {
	Name string
	Path string
}

// ChannelItem is a channel→project mapping row for the config UI.
type ChannelItem struct {
	ChannelID string
	Project   string
}

// Snapshot is a read-only copy of config fields used by the web UI.
type Snapshot struct {
	Projects            []ProjectItem
	Channels            []ChannelItem
	ProjectNames        []string
	AllowedUserIDs      []string
	AllowedRoleIDs      []string
	HTTPListen          string
	GrokBin             string
	Model               string
	MaxTurns            int
	Yolo                bool
	WorktreeIsolation   bool
	WorktreeIdleTTLDays int // effective value (default 30 when unset)
	AutoFixCI           bool
	AutoFixCIMax        int      // effective cap (default 2)
	RiskyPathGlobsText  string   // configured globs, one per line (empty if using defaults)
	RiskyPathUseDefault bool     // true when riskyPathGlobs is unset (nil)
	ClientID            string
	InviteURL           string
	InviteError         string
	InvitePermissions   int64
}

func (c *Config) YoloEnabled() bool {
	if c.Yolo == nil {
		return true
	}
	return *c.Yolo
}

func (c *Config) SummarizeTitleEnabled() bool {
	if c.SummarizeThreadTitle == nil {
		return true
	}
	return *c.SummarizeThreadTitle
}

func (c *Config) WorktreeIsolationEnabled() bool {
	if c.WorktreeIsolation == nil {
		return true
	}
	return *c.WorktreeIsolation
}

// WorktreeIdleTTLDaysValue returns the configured idle TTL in days.
// Omitted config uses DefaultWorktreeIdleTTLDays; 0 means cleanup is disabled.
func (c *Config) WorktreeIdleTTLDaysValue() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.WorktreeIdleTTLDays == nil {
		return DefaultWorktreeIdleTTLDays
	}
	return *c.WorktreeIdleTTLDays
}

// WorktreeIdleTTL returns the idle prune duration, or 0 when cleanup is disabled.
func (c *Config) WorktreeIdleTTL() time.Duration {
	days := c.WorktreeIdleTTLDaysValue()
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// RiskyPathGlobsConfigured reports whether riskyPathGlobs was set in config
// (including explicitly empty). Unset (nil) means use bot defaults.
func (c *Config) RiskyPathGlobsConfigured() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.RiskyPathGlobs != nil
}

// RiskyPathGlobsEffective returns configured globs, or nil when unset (caller uses defaults).
// An explicit empty list means "no risk flags".
func (c *Config) RiskyPathGlobsEffective() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.RiskyPathGlobs == nil {
		return nil // bot applies DefaultRiskyPathGlobs
	}
	return slices.Clone(c.RiskyPathGlobs)
}

// AutoFixCIEnabled is true only when autoFixCI is explicitly set true.
func (c *Config) AutoFixCIEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AutoFixCI != nil && *c.AutoFixCI
}

// AutoFixCIMaxAttempts returns the auto-fix cap (default 2, minimum 1 when auto-fix is used).
func (c *Config) AutoFixCIMaxAttempts() int {
	if c == nil {
		return 2
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AutoFixCIMax <= 0 {
		return 2
	}
	return c.AutoFixCIMax
}

// ListenAddr returns the HTTP bind address (env overrides config).
func (c *Config) ListenAddr() string {
	if v := strings.TrimSpace(os.Getenv("GROK_DISCORD_HTTP_LISTEN")); v != "" {
		return v
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.HTTPListen) != "" {
		return strings.TrimSpace(c.HTTPListen)
	}
	return defaultHTTPListen
}

func Load() (*Config, error) {
	path := os.Getenv("GROK_DISCORD_CONFIG")
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(wd, "config.json")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("missing config at %s (copy config.example.json → config.json): %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if token := os.Getenv("DISCORD_BOT_TOKEN"); token != "" {
		c.DiscordToken = token
	}
	if c.DiscordToken == "" || c.DiscordToken == "YOUR_BOT_TOKEN" {
		return nil, fmt.Errorf("set discordToken in config.json or DISCORD_BOT_TOKEN")
	}

	if c.Projects == nil {
		c.Projects = map[string]string{}
	}
	if c.Channels == nil {
		c.Channels = map[string]string{}
	}
	if len(c.Projects) == 0 {
		return nil, fmt.Errorf("config.projects must map project names → absolute paths")
	}
	if len(c.Channels) == 0 {
		return nil, fmt.Errorf("config.channels must map Discord channel IDs → project names")
	}

	for name, cwd := range c.Projects {
		if !filepath.IsAbs(cwd) {
			return nil, fmt.Errorf("project %q path must be absolute: %s", name, cwd)
		}
		if _, err := os.Stat(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] project %q path does not exist: %s\n", name, cwd)
		}
	}
	for ch, name := range c.Channels {
		if name == "" {
			return nil, fmt.Errorf("channels[%q] has empty project name", ch)
		}
		if _, ok := c.Projects[name]; !ok {
			return nil, fmt.Errorf("channels[%q] references unknown project %q", ch, name)
		}
	}

	if c.GrokBin == "" {
		c.GrokBin = "grok"
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = 40
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = 30 * 60 * 1000
	}
	if c.SummarizeTimeoutMs <= 0 {
		c.SummarizeTimeoutMs = 45_000
	}

	c.AllowedUsers = toSet(c.AllowedUserIDs)
	c.AllowedRoles = toSet(c.AllowedRoleIDs)
	c.ConfigPath = path
	c.DataDir = filepath.Join(filepath.Dir(path), "data")

	return &c, nil
}

// Save writes the mutable config fields back to ConfigPath.
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ConfigPath == "" {
		return fmt.Errorf("config path not set")
	}
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	// Re-read existing file so unknown/extra fields from other tools are not wiped
	// for keys we don't own; we rewrite the full known schema.
	out := struct {
		DiscordToken         string            `json:"discordToken"`
		DiscordClientID      string            `json:"discordClientId,omitempty"`
		AllowedUserIDs       []string          `json:"allowedUserIds"`
		AllowedRoleIDs       []string          `json:"allowedRoleIds"`
		Projects             map[string]string `json:"projects"`
		Channels             map[string]string `json:"channels"`
		GrokBin              string            `json:"grokBin"`
		Yolo                 *bool             `json:"yolo"`
		Model                string            `json:"model"`
		MaxTurns             int               `json:"maxTurns"`
		TimeoutMs            int               `json:"timeoutMs"`
		ExtraArgs            []string          `json:"extraArgs"`
		SummarizeThreadTitle *bool             `json:"summarizeThreadTitle"`
		SummarizeTimeoutMs   int               `json:"summarizeTimeoutMs"`
		WorktreeIsolation    *bool             `json:"worktreeIsolation"`
		WorktreeIdleTTLDays  *int              `json:"worktreeIdleTTLDays,omitempty"`
		HTTPListen           string            `json:"httpListen,omitempty"`
		RiskyPathGlobs       []string          `json:"riskyPathGlobs,omitempty"`
		AutoFixCI            *bool             `json:"autoFixCI,omitempty"`
		AutoFixCIMax         int               `json:"autoFixCIMax,omitempty"`
	}{
		DiscordToken:         c.DiscordToken,
		DiscordClientID:      c.DiscordClientID,
		AllowedUserIDs:       slices.Clone(c.AllowedUserIDs),
		AllowedRoleIDs:       slices.Clone(c.AllowedRoleIDs),
		Projects:             cloneStringMap(c.Projects),
		Channels:             cloneStringMap(c.Channels),
		GrokBin:              c.GrokBin,
		Yolo:                 c.Yolo,
		Model:                c.Model,
		MaxTurns:             c.MaxTurns,
		TimeoutMs:            c.TimeoutMs,
		ExtraArgs:            slices.Clone(c.ExtraArgs),
		SummarizeThreadTitle: c.SummarizeThreadTitle,
		SummarizeTimeoutMs:   c.SummarizeTimeoutMs,
		WorktreeIsolation:    c.WorktreeIsolation,
		WorktreeIdleTTLDays:  cloneIntPtr(c.WorktreeIdleTTLDays),
		HTTPListen:           c.HTTPListen,
		RiskyPathGlobs:       slices.Clone(c.RiskyPathGlobs),
		AutoFixCI:            c.AutoFixCI,
		AutoFixCIMax:         c.AutoFixCIMax,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(c.ConfigPath, raw, 0o600)
}

// SetWorktreeIdleTTLDays sets days of inactivity before worktree prune and persists.
// 0 disables automatic idle cleanup. Negative values are rejected.
func (c *Config) SetWorktreeIdleTTLDays(days int) error {
	if days < 0 {
		return fmt.Errorf("worktreeIdleTTLDays must be >= 0 (0 disables cleanup)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := days
	c.WorktreeIdleTTLDays = &d
	return c.saveLocked()
}

// SetAutoFixCI sets whether the PR poller auto-queues CI fixes and the per-session cap.
// maxAttempts <= 0 stores 0 (runtime still applies default 2 via AutoFixCIMaxAttempts).
func (c *Config) SetAutoFixCI(enabled bool, maxAttempts int) error {
	if maxAttempts < 0 {
		return fmt.Errorf("autoFixCIMax must be >= 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := enabled
	c.AutoFixCI = &v
	c.AutoFixCIMax = maxAttempts
	return c.saveLocked()
}

// SetRiskyPathGlobsFromText parses newline-separated globs.
// useDefault true clears the override (built-in defaults).
// useDefault false with empty text stores an empty list (no risk flags).
func (c *Config) SetRiskyPathGlobsFromText(text string, useDefault bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if useDefault {
		c.RiskyPathGlobs = nil
		return c.saveLocked()
	}
	var globs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		globs = append(globs, line)
	}
	if globs == nil {
		globs = []string{} // explicit empty ≠ nil defaults
	}
	c.RiskyPathGlobs = globs
	return c.saveLocked()
}

// AddProject registers a project folder (name → absolute path) and persists.
func (c *Config) AddProject(name, absPath string) error {
	name = strings.TrimSpace(name)
	absPath = strings.TrimSpace(absPath)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if absPath == "" {
		return fmt.Errorf("project path is required")
	}
	if !filepath.IsAbs(absPath) {
		return fmt.Errorf("project path must be absolute: %s", absPath)
	}
	absPath = filepath.Clean(absPath)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Projects == nil {
		c.Projects = map[string]string{}
	}
	if existing, ok := c.Projects[name]; ok {
		if existing == absPath {
			return nil
		}
		return fmt.Errorf("project %q already exists with path %s", name, existing)
	}
	c.Projects[name] = absPath
	return c.saveLocked()
}

// AddAllowedUser adds a Discord user ID to the allowlist and persists.
func (c *Config) AddAllowedUser(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("user id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.AllowedUsers[id]; ok {
		return nil
	}
	c.AllowedUserIDs = append(c.AllowedUserIDs, id)
	if c.AllowedUsers == nil {
		c.AllowedUsers = map[string]struct{}{}
	}
	c.AllowedUsers[id] = struct{}{}
	return c.saveLocked()
}

// AddAllowedRole adds a Discord role ID to the allowlist and persists.
func (c *Config) AddAllowedRole(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("role id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.AllowedRoles[id]; ok {
		return nil
	}
	c.AllowedRoleIDs = append(c.AllowedRoleIDs, id)
	if c.AllowedRoles == nil {
		c.AllowedRoles = map[string]struct{}{}
	}
	c.AllowedRoles[id] = struct{}{}
	return c.saveLocked()
}

// AddChannel maps a Discord channel ID to a project and persists.
// If the channel already maps to the same project, it is a no-op.
// If it maps to a different project, the mapping is updated.
func (c *Config) AddChannel(channelID, project string) error {
	channelID = strings.TrimSpace(channelID)
	project = strings.TrimSpace(project)
	if channelID == "" {
		return fmt.Errorf("channel id is required")
	}
	if project == "" {
		return fmt.Errorf("project name is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Projects[project]; !ok {
		return fmt.Errorf("unknown project %q", project)
	}
	if c.Channels == nil {
		c.Channels = map[string]string{}
	}
	if existing, ok := c.Channels[channelID]; ok && existing == project {
		return nil
	}
	c.Channels[channelID] = project
	return c.saveLocked()
}

// RemoveProject deletes a project and any channel mappings that point to it.
func (c *Config) RemoveProject(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Projects[name]; !ok {
		return fmt.Errorf("project %q not found", name)
	}
	delete(c.Projects, name)
	for ch, proj := range c.Channels {
		if proj == name {
			delete(c.Channels, ch)
		}
	}
	return c.saveLocked()
}

// RemoveAllowedUser removes a Discord user ID from the allowlist.
func (c *Config) RemoveAllowedUser(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("user id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.AllowedUsers[id]; !ok {
		return fmt.Errorf("user %q not found", id)
	}
	delete(c.AllowedUsers, id)
	c.AllowedUserIDs = removeString(c.AllowedUserIDs, id)
	return c.saveLocked()
}

// RemoveAllowedRole removes a Discord role ID from the allowlist.
func (c *Config) RemoveAllowedRole(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("role id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.AllowedRoles[id]; !ok {
		return fmt.Errorf("role %q not found", id)
	}
	delete(c.AllowedRoles, id)
	c.AllowedRoleIDs = removeString(c.AllowedRoleIDs, id)
	return c.saveLocked()
}

// RemoveChannel removes a channel→project mapping.
func (c *Config) RemoveChannel(channelID string) error {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return fmt.Errorf("channel id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Channels[channelID]; !ok {
		return fmt.Errorf("channel %q not found", channelID)
	}
	delete(c.Channels, channelID)
	return c.saveLocked()
}

// Snapshot returns a copy of UI-relevant config under read lock.
func (c *Config) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	slices.Sort(names)
	projects := make([]ProjectItem, 0, len(names))
	for _, n := range names {
		projects = append(projects, ProjectItem{Name: n, Path: c.Projects[n]})
	}

	chIDs := make([]string, 0, len(c.Channels))
	for id := range c.Channels {
		chIDs = append(chIDs, id)
	}
	slices.Sort(chIDs)
	channels := make([]ChannelItem, 0, len(chIDs))
	for _, id := range chIDs {
		channels = append(channels, ChannelItem{ChannelID: id, Project: c.Channels[id]})
	}

	idleDays := DefaultWorktreeIdleTTLDays
	if c.WorktreeIdleTTLDays != nil {
		idleDays = *c.WorktreeIdleTTLDays
	}
	autoFixMax := 2
	if c.AutoFixCIMax > 0 {
		autoFixMax = c.AutoFixCIMax
	}
	riskyDefault := c.RiskyPathGlobs == nil
	riskyText := ""
	if c.RiskyPathGlobs != nil {
		riskyText = strings.Join(c.RiskyPathGlobs, "\n")
	}
	snap := Snapshot{
		Projects:            projects,
		Channels:            channels,
		ProjectNames:        names,
		AllowedUserIDs:      slices.Clone(c.AllowedUserIDs),
		AllowedRoleIDs:      slices.Clone(c.AllowedRoleIDs),
		HTTPListen:          c.HTTPListen,
		GrokBin:             c.GrokBin,
		Model:               c.Model,
		MaxTurns:            c.MaxTurns,
		Yolo:                c.YoloEnabled(),
		WorktreeIsolation:   c.WorktreeIsolationEnabled(),
		WorktreeIdleTTLDays: idleDays,
		AutoFixCI:           c.AutoFixCI != nil && *c.AutoFixCI,
		AutoFixCIMax:        autoFixMax,
		RiskyPathGlobsText:  riskyText,
		RiskyPathUseDefault: riskyDefault,
		InvitePermissions:   BotInvitePermissions,
	}
	// ClientID/InviteURL may read DiscordClientID/DiscordToken; unlock first.
	// Snapshot already holds RLock — resolve invite without re-locking via local fields.
	explicit := strings.TrimSpace(c.DiscordClientID)
	token := c.DiscordToken
	id := explicit
	var idErr error
	if id == "" {
		id, idErr = ClientIDFromToken(token)
	}
	if idErr != nil {
		snap.InviteError = idErr.Error()
	} else {
		snap.ClientID = id
		snap.InviteURL = BuildInviteURL(id, BotInvitePermissions, BotInviteScopes)
	}
	return snap
}

func (c *Config) ProjectPath(name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.Projects[name]
	return p, ok
}

func (c *Config) ChannelProject(channelID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.Channels[channelID]
	return name, ok && name != ""
}

func (c *Config) UserAllowed(userID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.AllowedUsers[userID]
	return ok
}

func (c *Config) RoleAllowed(roleID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.AllowedRoles[roleID]
	return ok
}

func (c *Config) HasAllowlist() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.AllowedUsers) > 0 || len(c.AllowedRoles) > 0
}

func (c *Config) AllowlistSizes() (users, roles int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.AllowedUsers), len(c.AllowedRoles)
}

func (c *Config) ProjectNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (c *Config) ChannelCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.Channels)
}

func toSet(ids []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			m[id] = struct{}{}
		}
	}
	return m
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func removeString(ss []string, want string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != want {
			out = append(out, s)
		}
	}
	return out
}
