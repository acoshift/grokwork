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
	// DefaultRepoFetchIntervalMinutes is used when a project omits
	// repoFetchIntervalMinutes. Auto git fetch before new worktree create is
	// throttled to at most once per this many minutes per main checkout.
	DefaultRepoFetchIntervalMinutes = 5
	DefaultAutoFixCIMax             = 2
	DefaultMaxTurns                 = 40
	DefaultTimeoutMs                = 30 * 60 * 1000 // 30 minutes
	// DefaultBoardStaleDays is days of inactivity before a thread is "stale" on /board.
	DefaultBoardStaleDays = 3
	// MinTimeoutMs is the smallest allowed per-run timeout (1 second).
	MinTimeoutMs = 1000
	// MaxTimeoutMs caps the per-run timeout at 24 hours.
	MaxTimeoutMs = 24 * 60 * 60 * 1000
)

// DefaultRiskyPathGlobs flags completion-card paths that usually need careful review.
// Patterns use ** (any path prefix/suffix) and * (within one segment).
var DefaultRiskyPathGlobs = []string{
	"**/migrations/**",
	"**/migration/**",
	"**/*migration*",
	"**/auth/**",
	"**/deploy/**",
	"**/deployment/**",
	"**/.env",
	"**/.env.*",
	"**/secrets/**",
	"**/*secret*",
	"**/*credential*",
	"**/Dockerfile*",
	"**/*.tf",
	"**/k8s/**",
	"**/helm/**",
	"**/crdb/**",
	"**/gcp.json",
}

type Config struct {
	DiscordToken string `json:"discordToken"`
	// DiscordClientID is optional; when empty the client id is decoded from discordToken.
	DiscordClientID string `json:"discordClientId,omitempty"`
	// DiscordClientSecret is the OAuth2 client secret for web login (never log).
	// Prefer env DISCORD_CLIENT_SECRET / GROK_WORK_DISCORD_CLIENT_SECRET.
	DiscordClientSecret string            `json:"discordClientSecret,omitempty"`
	Projects            ProjectsMap       `json:"projects"`
	Channels            map[string]string `json:"channels"` // channel ID → project name
	GrokBin             string            `json:"grokBin"`
	Yolo                *bool             `json:"yolo"`
	Model               string            `json:"model"`
	MaxTurns            int               `json:"maxTurns"`
	TimeoutMs           int               `json:"timeoutMs"`
	ExtraArgs           []string          `json:"extraArgs"`
	SummarizeThreadTitle *bool            `json:"summarizeThreadTitle"`
	SummarizeTimeoutMs  int               `json:"summarizeTimeoutMs"`
	WorktreeIsolation   *bool             `json:"worktreeIsolation"`
	// WorktreeIdleTTLDays is days of inactivity before pruning thread worktrees.
	// nil/omitted → DefaultWorktreeIdleTTLDays (30). 0 disables idle cleanup.
	WorktreeIdleTTLDays *int `json:"worktreeIdleTTLDays,omitempty"`
	// HTTPListen is the address for the private-network web UI (e.g. ":8787", "0.0.0.0:8787").
	// Empty uses default ":8787". Override with GROK_WORK_HTTP_LISTEN.
	HTTPListen string `json:"httpListen,omitempty"`
	// WebPublicBaseURL is the absolute public origin for OAuth redirect_uri
	// (e.g. "http://100.x.y.z:8787"). Required when webAuth.enabled.
	WebPublicBaseURL string `json:"webPublicBaseURL,omitempty"`
	// DiscordGuildID is an optional default guild for Discord deep links when a
	// project does not set projects.<name>.discordGuildId (multi-guild deploy).
	DiscordGuildID string `json:"discordGuildId,omitempty"`
	// WebMergeMethod is the default gh pr merge strategy: squash (default), merge, rebase.
	WebMergeMethod string `json:"webMergeMethod,omitempty"`
	// WebAuth enables Discord OAuth for the private web UI. Nil/disabled = open LAN mode.
	WebAuth *WebAuthConfig `json:"webAuth,omitempty"`
	// RiskyPathGlobs flags completion-card paths for review (**, * globs).
	// nil/omitted → built-in defaults. Empty slice → no risk highlighting.
	RiskyPathGlobs []string `json:"riskyPathGlobs,omitempty"`
	// AutoFixCI queues a CI fix task when the PR status poller sees failing checks.
	// nil/omitted/false → digest only; user runs @Grok /fix-ci.
	AutoFixCI *bool `json:"autoFixCI,omitempty"`
	// AutoFixCIMax is the max auto-queued fix attempts per thread session (default 2).
	AutoFixCIMax int `json:"autoFixCIMax,omitempty"`
	// BoardStaleDays is days without session activity before /board lists a thread as stale.
	// nil/omitted → DefaultBoardStaleDays (3). Minimum 1.
	BoardStaleDays *int `json:"boardStaleDays,omitempty"`
	// BoardDigestChannel is an optional Discord channel ID for the nightly team board post.
	// Empty/omitted disables the digest.
	BoardDigestChannel string `json:"boardDigestChannel,omitempty"`
	// ResumeActiveRuns enables durable run journals and crash recovery.
	// nil/omitted → true. Explicit false disables (boot still purges leftover journals).
	ResumeActiveRuns *bool `json:"resumeActiveRuns,omitempty"`
	// ShutdownTimeoutMs is how long Bot.Stop waits for drains (default 15000).
	ShutdownTimeoutMs int `json:"shutdownTimeoutMs,omitempty"`

	mu         sync.RWMutex
	DataDir    string `json:"-"`
	ConfigPath string `json:"-"`

	catalogMu    sync.Mutex
	catalogCache map[string]catalogCacheEntry
}

// ProjectItem is a project row for the config UI.
type ProjectItem struct {
	Name                     string
	Path                     string
	LinearEnabled            bool
	LinearTeamKey            string
	LinearAPIKeySet          bool   // true when config or env has a key (never expose the secret)
	LinearEnvHint            string // e.g. LINEAR_API_KEY_HOMECONNECT
	DiscordChannelID         string
	DiscordGuildID           string
	GitHubReposText           string // "owner/repo" lines for config form
	ChannelOptions           []string // channel IDs mapped to this project (preferred dropdown)
	AllowedUserIDs           []string
	AllowedRoleIDs           []string
	RepoFetchIntervalMinutes int // effective minutes (default when unset; 0 = disabled)
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
	HTTPListen          string
	GrokBin             string
	Model               string
	MaxTurns            int // effective (default 40)
	TimeoutMs           int // effective (default 1800000 = 30m)
	Yolo                bool
	WorktreeIsolation   bool
	WorktreeIdleTTLDays int // effective value (default 30 when unset)
	AutoFixCI           bool
	AutoFixCIMax        int    // effective cap (default 2)
	RiskyPathGlobsText  string // configured globs, one per line (empty if using defaults)
	RiskyPathUseDefault bool   // true when riskyPathGlobs is unset (nil)
	BoardStaleDays      int    // effective (default 3)
	BoardDigestChannel  string // empty = digest disabled
	ResumeActiveRuns    bool   // effective (default true)
	ShutdownTimeoutMs   int    // effective (default 15000 when unset)
	ClientID            string
	InviteURL           string
	InviteError         string
	InvitePermissions   int64
	// Web auth (no secrets).
	WebAuthEnabled bool
	WebAuthRole    string // empty in snapshot; filled by web layer per-request
	DiscordGuildID string
	WebMergeMethod string // effective default (squash)
	// Feature flags for UI (true only when webAuth enabled + feature bit).
	FeatureGitHubWrites bool
	FeatureMerge        bool
}

// DefaultShutdownTimeoutMs is used when shutdownTimeoutMs is unset/invalid.
const DefaultShutdownTimeoutMs = 15000

// ResumeActiveRunsEnabled reports whether crash-safe resume is on (nil → true).
func (c *Config) ResumeActiveRunsEnabled() bool {
	if c == nil || c.ResumeActiveRuns == nil {
		return true
	}
	return *c.ResumeActiveRuns
}

// ShutdownTimeoutMsValue returns the Stop drain wait in ms (default 15000).
func (c *Config) ShutdownTimeoutMsValue() int {
	if c == nil || c.ShutdownTimeoutMs <= 0 {
		return DefaultShutdownTimeoutMs
	}
	return c.ShutdownTimeoutMs
}

// SetResumeActiveRuns sets the resume flag and persists.
func (c *Config) SetResumeActiveRuns(enabled bool) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := enabled
	c.ResumeActiveRuns = &v
	return c.saveLocked()
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

// AutoFixCIMaxAttempts returns the auto-fix cap (default DefaultAutoFixCIMax).
func (c *Config) AutoFixCIMaxAttempts() int {
	if c == nil {
		return DefaultAutoFixCIMax
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AutoFixCIMax <= 0 {
		return DefaultAutoFixCIMax
	}
	return c.AutoFixCIMax
}

// BoardStaleDaysValue returns days of inactivity for the /board stale bucket.
// Omitted config uses DefaultBoardStaleDays; values < 1 fall back to the default.
func (c *Config) BoardStaleDaysValue() int {
	if c == nil {
		return DefaultBoardStaleDays
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.BoardStaleDays == nil || *c.BoardStaleDays < 1 {
		return DefaultBoardStaleDays
	}
	return *c.BoardStaleDays
}

// BoardDigestChannelValue returns the Discord channel ID for the nightly board digest, or "".
func (c *Config) BoardDigestChannelValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.BoardDigestChannel)
}

// SetBoardSettings sets board stale threshold and optional digest channel, then persists.
// staleDays must be >= 1. digestChannel empty disables the nightly post.
func (c *Config) SetBoardSettings(staleDays int, digestChannel string) error {
	if staleDays < 1 {
		return fmt.Errorf("boardStaleDays must be >= 1")
	}
	digestChannel = strings.TrimSpace(digestChannel)
	c.mu.Lock()
	defer c.mu.Unlock()
	d := staleDays
	c.BoardStaleDays = &d
	c.BoardDigestChannel = digestChannel
	return c.saveLocked()
}

// ListenAddr returns the HTTP bind address (env overrides config).
func (c *Config) ListenAddr() string {
	if v := EnvWork("HTTP_LISTEN"); v != "" {
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
	path := EnvWork("CONFIG")
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
		c.Projects = ProjectsMap{}
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

	for name, pc := range c.Projects {
		cwd := pc.Path
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
		c.MaxTurns = DefaultMaxTurns
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = DefaultTimeoutMs
	}
	if c.SummarizeTimeoutMs <= 0 {
		c.SummarizeTimeoutMs = 45_000
	}

	c.ConfigPath = path
	c.DataDir = filepath.Join(filepath.Dir(path), "data")

	c.applyWebAuthBootstrap()
	if err := c.ValidateWebAuth(); err != nil {
		return nil, err
	}
	if err := c.ValidatePreferredChannels(); err != nil {
		return nil, err
	}

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
		DiscordClientSecret  string            `json:"discordClientSecret,omitempty"`
		Projects             ProjectsMap       `json:"projects"`
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
		WebPublicBaseURL     string            `json:"webPublicBaseURL,omitempty"`
		DiscordGuildID       string            `json:"discordGuildId,omitempty"`
		WebMergeMethod       string            `json:"webMergeMethod,omitempty"`
		WebAuth              *WebAuthConfig    `json:"webAuth,omitempty"`
		RiskyPathGlobs       []string          `json:"riskyPathGlobs,omitempty"`
		AutoFixCI            *bool             `json:"autoFixCI,omitempty"`
		AutoFixCIMax         int               `json:"autoFixCIMax,omitempty"`
		BoardStaleDays       *int              `json:"boardStaleDays,omitempty"`
		BoardDigestChannel   string            `json:"boardDigestChannel,omitempty"`
		ResumeActiveRuns     *bool             `json:"resumeActiveRuns,omitempty"`
		ShutdownTimeoutMs    int               `json:"shutdownTimeoutMs,omitempty"`
	}{
		DiscordToken:         c.DiscordToken,
		DiscordClientID:      c.DiscordClientID,
		DiscordClientSecret:  c.DiscordClientSecret,
		Projects:             cloneProjectsMap(c.Projects),
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
		WebPublicBaseURL:     c.WebPublicBaseURL,
		DiscordGuildID:       c.DiscordGuildID,
		WebMergeMethod:       c.WebMergeMethod,
		WebAuth:              cloneWebAuth(c.WebAuth),
		RiskyPathGlobs:       slices.Clone(c.RiskyPathGlobs),
		AutoFixCI:            c.AutoFixCI,
		AutoFixCIMax:         c.AutoFixCIMax,
		BoardStaleDays:       cloneIntPtr(c.BoardStaleDays),
		BoardDigestChannel:   c.BoardDigestChannel,
		ResumeActiveRuns:     cloneBoolPtr(c.ResumeActiveRuns),
		ShutdownTimeoutMs:    c.ShutdownTimeoutMs,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(c.ConfigPath, raw, 0o600)
}

// MaxTurnsValue returns the per-run turn cap (default DefaultMaxTurns when unset/invalid).
func (c *Config) MaxTurnsValue() int {
	if c == nil {
		return DefaultMaxTurns
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxTurns <= 0 {
		return DefaultMaxTurns
	}
	return c.MaxTurns
}

// TimeoutMsValue returns the per-run timeout in milliseconds (default DefaultTimeoutMs).
func (c *Config) TimeoutMsValue() int {
	if c == nil {
		return DefaultTimeoutMs
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.TimeoutMs <= 0 {
		return DefaultTimeoutMs
	}
	return c.TimeoutMs
}

// SetGrokRunLimits sets maxTurns and timeoutMs for Grok task runs and persists.
// maxTurns must be >= 1; timeoutMs must be in [MinTimeoutMs, MaxTimeoutMs].
// Applies to subsequent runs (in-flight runs keep their limits).
func (c *Config) SetGrokRunLimits(maxTurns, timeoutMs int) error {
	if maxTurns < 1 {
		return fmt.Errorf("maxTurns must be >= 1")
	}
	if timeoutMs < MinTimeoutMs {
		return fmt.Errorf("timeoutMs must be >= %d (1 second)", MinTimeoutMs)
	}
	if timeoutMs > MaxTimeoutMs {
		return fmt.Errorf("timeoutMs must be <= %d (24 hours)", MaxTimeoutMs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MaxTurns = maxTurns
	c.TimeoutMs = timeoutMs
	return c.saveLocked()
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
// maxAttempts must be >= 1.
func (c *Config) SetAutoFixCI(enabled bool, maxAttempts int) error {
	if maxAttempts < 1 {
		return fmt.Errorf("autoFixCIMax must be >= 1")
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
		c.Projects = ProjectsMap{}
	}
	if existing, ok := c.Projects[name]; ok {
		if existing.Path == absPath {
			return nil
		}
		return fmt.Errorf("project %q already exists with path %s", name, existing.Path)
	}
	c.Projects[name] = ProjectConfig{Path: absPath}
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
		pc := c.Projects[n]
		fetchMins := DefaultRepoFetchIntervalMinutes
		if pc.RepoFetchIntervalMinutes != nil {
			if *pc.RepoFetchIntervalMinutes < 0 {
				fetchMins = DefaultRepoFetchIntervalMinutes
			} else {
				fetchMins = *pc.RepoFetchIntervalMinutes
			}
		}
		item := ProjectItem{
			Name:                     n,
			Path:                     pc.Path,
			LinearEnvHint:            "LINEAR_API_KEY_" + ProjectEnvKeySuffix(n),
			DiscordChannelID:         strings.TrimSpace(pc.DiscordChannelID),
			DiscordGuildID:           strings.TrimSpace(pc.DiscordGuildID),
			AllowedUserIDs:           slices.Clone(pc.AllowedUserIDs),
			AllowedRoleIDs:           slices.Clone(pc.AllowedRoleIDs),
			RepoFetchIntervalMinutes: fetchMins,
		}
		if pc.Linear != nil {
			item.LinearEnabled = pc.Linear.Enabled
			item.LinearTeamKey = strings.TrimSpace(pc.Linear.TeamKey)
			item.LinearAPIKeySet = strings.TrimSpace(pc.Linear.APIKey) != "" || linearAPIKeyFromEnv(n) != ""
		} else if linearAPIKeyFromEnv(n) != "" {
			item.LinearAPIKeySet = true
		}
		if repos := pc.GitHub.NormalizedRepos(); len(repos) > 0 {
			lines := make([]string, 0, len(repos))
			for _, r := range repos {
				lines = append(lines, r.Slug())
			}
			item.GitHubReposText = strings.Join(lines, "\n")
		}
		for ch, proj := range c.Channels {
			if proj == n {
				item.ChannelOptions = append(item.ChannelOptions, ch)
			}
		}
		slices.Sort(item.ChannelOptions)
		projects = append(projects, item)
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
	autoFixMax := DefaultAutoFixCIMax
	if c.AutoFixCIMax > 0 {
		autoFixMax = c.AutoFixCIMax
	}
	boardStale := DefaultBoardStaleDays
	if c.BoardStaleDays != nil && *c.BoardStaleDays >= 1 {
		boardStale = *c.BoardStaleDays
	}
	// When using built-in defaults, still show them in the UI so unchecking
	// "use defaults" does not save an empty list (which disables risk flags).
	riskyDefault := c.RiskyPathGlobs == nil
	riskyText := ""
	if c.RiskyPathGlobs != nil {
		riskyText = strings.Join(c.RiskyPathGlobs, "\n")
	} else {
		riskyText = strings.Join(DefaultRiskyPathGlobs, "\n")
	}
	maxTurns := c.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	timeoutMs := c.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = DefaultTimeoutMs
	}
	snap := Snapshot{
		Projects:            projects,
		Channels:            channels,
		ProjectNames:        names,
		HTTPListen:          c.HTTPListen,
		GrokBin:             c.GrokBin,
		Model:               c.Model,
		MaxTurns:            maxTurns,
		TimeoutMs:           timeoutMs,
		Yolo:                c.YoloEnabled(),
		WorktreeIsolation:   c.WorktreeIsolationEnabled(),
		WorktreeIdleTTLDays: idleDays,
		AutoFixCI:           c.AutoFixCI != nil && *c.AutoFixCI,
		AutoFixCIMax:        autoFixMax,
		RiskyPathGlobsText:  riskyText,
		RiskyPathUseDefault: riskyDefault,
		BoardStaleDays:      boardStale,
		BoardDigestChannel:  strings.TrimSpace(c.BoardDigestChannel),
		ResumeActiveRuns:    c.ResumeActiveRuns == nil || *c.ResumeActiveRuns,
		ShutdownTimeoutMs: func() int {
			if c.ShutdownTimeoutMs <= 0 {
				return DefaultShutdownTimeoutMs
			}
			return c.ShutdownTimeoutMs
		}(),
		InvitePermissions: BotInvitePermissions,
		WebAuthEnabled:    c.WebAuth != nil && c.WebAuth.Enabled,
		DiscordGuildID:    strings.TrimSpace(c.DiscordGuildID),
		WebMergeMethod:    c.webMergeMethodLocked(),
	}
	// Features need WebAuthEnabled without re-locking — compute inline.
	if c.WebAuth != nil && c.WebAuth.Enabled {
		snap.FeatureGitHubWrites = c.WebAuth.Features.GitHubWrites
		snap.FeatureMerge = c.WebAuth.Features.Merge
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
	pc, ok := c.Projects[name]
	if !ok {
		return "", false
	}
	return pc.Path, true
}

func (c *Config) ChannelProject(channelID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.Channels[channelID]
	return name, ok && name != ""
}

// EmptyProjectsCount returns how many projects have no members (user or role).
func (c *Config) EmptyProjectsCount() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, pc := range c.Projects {
		if !projectHasAllowlist(pc) {
			n++
		}
	}
	return n
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

func cloneBoolPtr(p *bool) *bool {
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
