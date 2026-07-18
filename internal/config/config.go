package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const defaultHTTPListen = ":8787"

type Config struct {
	DiscordToken string `json:"discordToken"`
	// DiscordClientID is optional; when empty the client id is decoded from discordToken.
	DiscordClientID string            `json:"discordClientId,omitempty"`
	AllowedUserIDs  []string          `json:"allowedUserIds"`
	AllowedRoleIDs  []string          `json:"allowedRoleIds"`
	Projects        map[string]string `json:"projects"`
	Channels        map[string]string `json:"channels"` // channel ID → project name
	GrokBin         string            `json:"grokBin"`
	Yolo            *bool             `json:"yolo"`
	Model           string            `json:"model"`
	MaxTurns        int               `json:"maxTurns"`
	TimeoutMs       int               `json:"timeoutMs"`
	ExtraArgs            []string `json:"extraArgs"`
	SummarizeThreadTitle *bool    `json:"summarizeThreadTitle"`
	SummarizeTimeoutMs   int      `json:"summarizeTimeoutMs"`
	WorktreeIsolation    *bool    `json:"worktreeIsolation"`
	// HTTPListen is the address for the private-network web UI (e.g. ":8787", "0.0.0.0:8787").
	// Empty uses default ":8787". Override with GROK_DISCORD_HTTP_LISTEN.
	HTTPListen string `json:"httpListen,omitempty"`

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
	Projects          []ProjectItem
	Channels          []ChannelItem
	ProjectNames      []string
	AllowedUserIDs    []string
	AllowedRoleIDs    []string
	HTTPListen        string
	GrokBin           string
	Model             string
	MaxTurns          int
	Yolo              bool
	WorktreeIsolation bool
	ClientID          string
	InviteURL         string
	InviteError       string
	InvitePermissions int64
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
		HTTPListen           string            `json:"httpListen,omitempty"`
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
		HTTPListen:           c.HTTPListen,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(c.ConfigPath, raw, 0o600)
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

	snap := Snapshot{
		Projects:          projects,
		Channels:          channels,
		ProjectNames:      names,
		AllowedUserIDs:    slices.Clone(c.AllowedUserIDs),
		AllowedRoleIDs:    slices.Clone(c.AllowedRoleIDs),
		HTTPListen:        c.HTTPListen,
		GrokBin:           c.GrokBin,
		Model:             c.Model,
		MaxTurns:          c.MaxTurns,
		Yolo:              c.YoloEnabled(),
		WorktreeIsolation: c.WorktreeIsolationEnabled(),
		InvitePermissions: BotInvitePermissions,
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

func removeString(ss []string, want string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != want {
			out = append(out, s)
		}
	}
	return out
}
