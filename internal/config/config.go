package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
	"github.com/acoshift/grokwork/internal/grokrun"
)

const (
	defaultHTTPListen          = ":8787"
	DefaultWorktreeIdleTTLDays = 30
	// DefaultTerminalSessionTTLDays: unset config means disabled (0).
	DefaultTerminalSessionTTLDays = 0
	// DefaultRepoFetchIntervalMinutes is used when a project omits
	// repoFetchIntervalMinutes. Idle background git fetch is throttled to at
	// most once per this many minutes per main checkout. Worktree create uses
	// a separate hardcoded 5s throttle (gitworktree.CreateFetchThrottle).
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
	// Agent is the default coding CLI for new sessions: "grok" (default),
	// "claude", or "cursor". Sessions stamp the agent they were created with
	// and keep it, since a session id cannot be resumed by the other CLI.
	Agent string `json:"agent,omitempty"`
	// ClaudeBin is the claude CLI binary. Empty → "claude" on PATH.
	ClaudeBin string `json:"claudeBin,omitempty"`
	// CursorBin is the cursor-agent CLI binary. Empty → "cursor-agent" on PATH.
	CursorBin string `json:"cursorBin,omitempty"`
	// SummarizeModel overrides Model for thread-title summarization only. That
	// call is one turn with tools off and returns a few words, so it is worth
	// pointing at a cheap model. Like Model it takes a name from either vendor
	// and the agent is inferred from it. Empty → Model.
	SummarizeModel string `json:"summarizeModel,omitempty"`
	// ReviewModel is the default model for review sessions started from the web
	// (commit review, address CI/review) — read-heavy, judgement-heavy work worth
	// pointing at a stronger model than everyday tasks. It is only a default: the
	// dispatch UI can override it per session, and the choice is stamped on the
	// session like any other. Empty → Model.
	ReviewModel string `json:"reviewModel,omitempty"`
	// ModelRates prices token usage per model, in dollars per million tokens,
	// keyed by normalized model name (see ModelRateKey). Absent or partial rates
	// report tokens without a dollar figure rather than a wrong one — see
	// ModelRate.Price.
	ModelRates map[string]ModelRate `json:"modelRates,omitempty"`
	// ClaudeExtraArgs are extra claude CLI flags. ExtraArgs below is grok
	// vocabulary and is never passed to claude.
	ClaudeExtraArgs []string `json:"claudeExtraArgs,omitempty"`
	// ClaudeIncludeAnthropicEnv keeps ANTHROPIC_* in the claude child environment.
	// nil/false strips them like every other host credential. An OAuth/keychain
	// login needs nothing here; enable it only for API-key or gateway setups.
	ClaudeIncludeAnthropicEnv *bool `json:"claudeIncludeAnthropicEnv,omitempty"`
	Yolo                      *bool `json:"yolo"`
	// Model is the task model for either agent. The agent that owns the name is
	// inferred (grokrun.AgentForModel); an unrecognized name falls back to Agent.
	Model                string   `json:"model"`
	MaxTurns             int      `json:"maxTurns"`
	TimeoutMs            int      `json:"timeoutMs"`
	ExtraArgs            []string `json:"extraArgs"`
	SummarizeThreadTitle *bool    `json:"summarizeThreadTitle"`
	SummarizeTimeoutMs   int      `json:"summarizeTimeoutMs"`
	WorktreeIsolation    *bool    `json:"worktreeIsolation"`
	// WorktreeDir is the root directory for per-thread git worktrees
	// (<root>/<project>/<unitID>). Empty/omitted → <DataDir>/worktrees.
	// Absolute, or relative to the config file directory. Applies to newly
	// created worktrees; existing sessions keep their stored cwd until reset.
	WorktreeDir string `json:"worktreeDir,omitempty"`
	// WorktreeIdleTTLDays is days of inactivity before pruning thread worktrees.
	// nil/omitted → DefaultWorktreeIdleTTLDays (30). 0 disables idle cleanup.
	WorktreeIdleTTLDays *int `json:"worktreeIdleTTLDays,omitempty"`
	// TerminalSessionTTLDays is days since last update before deleting
	// done/abandoned session tombstones. nil/omitted → 0 (disabled). 0 disables.
	TerminalSessionTTLDays *int `json:"terminalSessionTTLDays,omitempty"`
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
	// DiscordPRLink chooses which URL is posted in Discord PR cards/status/events:
	// "github" (default) → https://github.com/owner/repo/pull/N
	// "web" → {webPublicBaseURL}/prs/owner/repo/N (falls back to GitHub when base URL unset).
	DiscordPRLink string `json:"discordPRLink,omitempty"`
	// WebAuth enables Discord OAuth for the private web UI. Nil/disabled = open LAN mode.
	WebAuth *WebAuthConfig `json:"webAuth,omitempty"`
	// API is the machine-to-machine HTTP API. Nil/omitted/disabled = no /api/v1 routes.
	API *APIConfig `json:"api,omitempty"`
	// RiskyPathGlobs flags completion-card paths for review (**, * globs).
	// nil/omitted → built-in defaults. Empty slice → no risk highlighting.
	RiskyPathGlobs []string `json:"riskyPathGlobs,omitempty"`
	// AutoFixCI queues a CI fix task when the PR status poller sees failing checks.
	// nil/omitted/false → digest only; user runs @Grok /fix-ci.
	AutoFixCI *bool `json:"autoFixCI,omitempty"`
	// AgentMCP enables in-session grokwork MCP inject for unrestricted runs (default true).
	// Explicit false is the kill-switch.
	AgentMCP *bool `json:"agentMCP,omitempty"`
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
	// MaxConcurrentRuns is host-wide active Grok runs (nil/0 = unlimited).
	MaxConcurrentRuns *int `json:"maxConcurrentRuns,omitempty"`
	// MaxConcurrentRunsUser is per-actor concurrent runs (nil/0 = unlimited).
	MaxConcurrentRunsUser *int `json:"maxConcurrentRunsUser,omitempty"`
	// GrokEnvDenylist is extra env var name prefixes stripped from Grok children (Layer A).
	GrokEnvDenylist []string `json:"grokEnvDenylist,omitempty"`
	// NotifyOnDone controls when the run author is @mentioned after a Grok run:
	// never | errors | always | long_only. Empty/omitted → errors.
	// MaxConcurrentDeploys caps host-wide concurrent deploy runs.
	// nil/0 → DefaultMaxConcurrentDeploys.
	MaxConcurrentDeploys *int `json:"maxConcurrentDeploys,omitempty"`
	// DeployRunRetention is how many terminal deploy runs to keep per lane.
	// nil/0 → DefaultDeployRunRetention.
	DeployRunRetention *int `json:"deployRunRetention,omitempty"`

	NotifyOnDone string `json:"notifyOnDone,omitempty"`
	// NotifyOnDoneLongMs is the elapsed threshold for long_only (default 300000 = 5m).
	NotifyOnDoneLongMs int `json:"notifyOnDoneLongMs,omitempty"`
	// Storage is the host-wide GCS default for project Files pages. Projects
	// without an override inherit it under an isolated prefix segment. nil =
	// no default (each project must set its own storage or have none).
	Storage *StorageConfig `json:"storage,omitempty"`

	mu         sync.RWMutex
	DataDir    string `json:"-"`
	ConfigPath string `json:"-"`

	catalogMu    sync.Mutex
	catalogCache map[string]catalogCacheEntry
}

// CapabilityMapItem is a user → template row for the project config UI.
type CapabilityMapItem struct {
	ID       string
	Template string
}

// TeamItem is one team row for the project config UI.
type TeamItem struct {
	Key             string
	Label           string // display; falls back to Key when empty
	Capabilities    string // template name; "" = default fallback
	TemplateUnknown bool   // named a template that resolves to nothing
	Members         []string
}

// ProjectItem is a project row for the config UI.
type ProjectItem struct {
	Name                  string
	Path                  string
	LinearEnabled         bool
	LinearTeamKey         string
	LinearAPIKeySet       bool   // true when config or env has a key (never expose the secret)
	LinearEnvHint         string // e.g. LINEAR_API_KEY_HOMECONNECT
	ClickUpEnabled        bool
	ClickUpWorkspaceID    string
	ClickUpListID         string
	ClickUpCustomIdPrefix string
	ClickUpAPIKeySet      bool
	ClickUpEnvHint        string // e.g. CLICKUP_API_KEY_APP
	GCPErrorsEnabled      bool
	GCPProjectID          string
	GCPProjectNumber      string
	GCPService            string
	GCPCredentialsFile    string // path only; never key contents
	SentryEnabled         bool
	SentryOrg             string
	SentryProject         string
	SentryBaseURL         string
	SentryAuthTokenSet    bool
	SentryEnvHint         string // SENTRY_AUTH_TOKEN_<SUFFIX>
	DeploysErrorsEnabled  bool
	DeploysErrorsProject  string
	DeploysErrorsLocation string
	DeploysErrorsName     string
	DeploysAPITokenSet    bool
	DeploysEnvHint        string // DEPLOYS_API_TOKEN_<SUFFIX> — not DEPLOYS_TOKEN
	DiscordChannelID      string
	DiscordGuildID        string
	GitHubReposText       string   // "owner/repo" lines for config form
	ChannelOptions        []string // channel IDs mapped to this project (preferred dropdown)
	AllowedUserIDs        []string
	// Teams are this project's teams, sorted by key.
	Teams []TeamItem
	// MemberIDs is the union of allowedUserIds and every team's members
	// (normalized, sorted, deduped) — "who is on this project".
	MemberIDs                []string
	RepoFetchIntervalMinutes int  // effective minutes (default when unset; 0 = disabled)
	DirectToPrimary          bool // true when project ships without PRs
	AgentMCPAlways           bool // true when Grok investigate/plan also attach grokwork MCP
	// Safe team / capabilities (K16)
	SafeTeamMode            bool
	SafeTeamDefaultTemplate string // effective (default "investigator")
	DefaultMode             string // empty = legacy fix
	// CaseKey is the configured case-id prefix override, empty when derived.
	CaseKey string
	// PrimaryBranch is the raw projects.*.primaryBranch (may be invalid for repair).
	PrimaryBranch string
	// PrimaryBranchInvalid is true when raw PrimaryBranch fails validation.
	PrimaryBranchInvalid bool
	// SLA is one settings-form row per severity (SLASeverities order); empty
	// minutes mean that clock has no target. SLAConfigured is true when at
	// least one row has one.
	SLA                     []SLAItem
	SLAConfigured           bool
	CapabilityByUser        []CapabilityMapItem
	UnmappedUserIDs         []string // allowlisted users with no capabilityByUser entry
	CapabilityTemplateNames []string // builtin + project overlay names for selects
	// VerifyCommandsText is the config form body: "name | command [| timeoutMs]" per line.
	VerifyCommandsText string
	// Deploy settings. DeployEnvs carries variable NAMES and a secret flag only —
	// never a value, the same rule as LinearAPIKeySet above.
	DeployEnabled      bool
	DeployManifestPath string
	DeployEnvs         []DeployEnvItem
	// ActionsRules are the branch-lock rules for workflow_dispatch (Integrations).
	ActionsRules []ActionsDispatchRule
	// StorageBucket / StoragePrefix are the raw project override (empty when
	// nil or disabled). The bucket name is not a secret and may be shown
	// verbatim. Effective target is StorageEffective* / StorageSource.
	StorageBackend       string // "gcs" | "gdrive" | "" (nil/disabled)
	StorageBucket        string
	StoragePrefix        string
	StorageDriveFolderID string
	// StorageCredentialsFile is the raw override key path (empty = inherit
	// global, then host gcloud auth). A local path — fine for the private
	// web UI, never for Discord or audit details; the key contents never
	// enter config at all.
	StorageCredentialsFile string
	// StorageDisabled is true when the project block has disabled:true.
	StorageDisabled bool
	// StorageInherited is true when the project has no override and a global
	// default is configured (will inherit unless disabled).
	StorageInherited bool
	// StorageEffective* are from EffectiveStorage.
	StorageEffectiveBackend       string
	StorageEffectiveBucket        string
	StorageEffectivePrefix        string
	StorageEffectiveDriveFolderID string
	StorageEffectiveIsolation     string // IsolationSegment when Drive inherit
	// StorageSource is "override" | "global" | "disabled" | "none".
	StorageSource string
}

// ChannelItem is a channel→project mapping row for the config UI.
type ChannelItem struct {
	ChannelID string
	Project   string
}

// Snapshot is a read-only copy of config fields used by the web UI.
type Snapshot struct {
	Projects     []ProjectItem
	Channels     []ChannelItem
	ProjectNames []string
	HTTPListen   string
	GrokBin      string
	Model        string
	// Agent is the default coding CLI for new sessions ("grok", "claude", or "cursor").
	Agent     string
	ClaudeBin string
	CursorBin string
	// SummarizeModel / ReviewModel are the raw configured values (empty = "use
	// Model"), not the effective ones, so the config form shows a placeholder
	// rather than a fabricated value.
	SummarizeModel string
	ReviewModel    string
	// ModelAgent / SummarizeModelAgent / ReviewModelAgent are the agents inferred
	// from those model names, and *Known is false when the name identifies neither.
	// The config page renders these so the derived agent is visible rather than
	// implicit.
	ModelAgent          string
	ModelAgentKnown     bool
	SummarizeAgent      string
	SummarizeAgentKnown bool
	ReviewAgent         string
	ReviewAgentKnown    bool
	// ModelLimits / SummarizeLimits / ReviewLimits are the currently selected
	// (or fallback) CLI's harness caveats, so the config page can paint them
	// without a second lookup. FallbackLimits is the empty "CLI default"
	// option — the configured agent, not the inferred one.
	ModelLimits     string
	SummarizeLimits string
	ReviewLimits    string
	FallbackLimits  string
	// ModelGroups / SummarizeModelGroups / ReviewModelGroups are the dropdown
	// options for each field, grouped by agent and including the configured value
	// when it is not curated.
	ModelGroups          []ModelGroup
	SummarizeModelGroups []ModelGroup
	ReviewModelGroups    []ModelGroup
	// ModelRates is the per-model price table for the config form (curated models
	// first, then any extra name config already carries). ModelRatesSet counts the
	// rows with at least one figure, which is what the hub row reports — a spend
	// report with zero configured rates shows tokens only, and that is worth
	// surfacing before someone goes looking for the dollars.
	ModelRates                []ModelRateItem
	ModelRatesSet             int
	ClaudeIncludeAnthropicEnv bool
	MaxTurns                  int // effective (default 40)
	TimeoutMs                 int // effective (default 1800000 = 30m)
	// MaxConcurrentRuns / MaxConcurrentRunsUser are the host-wide / per-actor
	// active-run caps (0 = unlimited; nil and 0 collapse to the same value here).
	MaxConcurrentRuns     int
	MaxConcurrentRunsUser int
	Yolo                  bool
	WorktreeIsolation     bool
	// WorktreeDir is the configured override (empty = default under DataDir).
	WorktreeDir string
	// WorktreesRoot is the effective absolute root for new worktrees.
	WorktreesRoot       string
	WorktreeIdleTTLDays int // effective value (default 30 when unset)
	// TerminalSessionTTLDays effective (0 = disabled when unset).
	TerminalSessionTTLDays int
	AutoFixCI              bool
	AutoFixCIMax           int    // effective cap (default 2)
	RiskyPathGlobsText     string // configured globs, one per line (empty if using defaults)
	RiskyPathUseDefault    bool   // true when riskyPathGlobs is unset (nil)
	BoardStaleDays         int    // effective (default 3)
	BoardDigestChannel     string // empty = digest disabled
	ResumeActiveRuns       bool   // effective (default true)
	ShutdownTimeoutMs      int    // effective (default 15000 when unset)
	ClientID               string
	InviteURL              string
	InviteError            string
	InvitePermissions      int64
	// Web auth (no secrets).
	WebAuthEnabled bool
	WebAuthRole    string // empty in snapshot; filled by web layer per-request
	DiscordGuildID string
	WebMergeMethod string // effective default (squash)
	// DiscordPRLink is "github" or "web" (how PR URLs appear in Discord messages).
	DiscordPRLink string
	// WebPublicBaseURL is the public origin used when DiscordPRLink is "web" (may be empty).
	WebPublicBaseURL string
	// Feature flags for UI (true only when webAuth enabled + feature bit).
	FeatureGitHubWrites bool
	FeatureMerge        bool
	FeaturePRReviews    bool
	// NotifyOnDone effective: never | errors | always | long_only.
	NotifyOnDone string
	// NotifyOnDoneLongMs effective long_only threshold.
	NotifyOnDoneLongMs int
	// Host-wide storage default (raw). StorageConfigured is identity-based.
	StorageBackend         string // "gcs" | "gdrive" | ""
	StorageBucket          string
	StoragePrefix          string
	StorageDriveFolderID   string
	StorageCredentialsFile string
	StorageConfigured      bool
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

// TerminalSessionTTLDaysValue returns days before done/abandoned sessions are deleted.
// Omitted config uses 0 (disabled). 0 means cleanup is disabled.
func (c *Config) TerminalSessionTTLDaysValue() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.TerminalSessionTTLDays == nil {
		return DefaultTerminalSessionTTLDays
	}
	return *c.TerminalSessionTTLDays
}

// TerminalSessionTTL returns the terminal-session prune duration, or 0 when disabled.
func (c *Config) TerminalSessionTTL() time.Duration {
	days := c.TerminalSessionTTLDaysValue()
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

// AgentMCPEnabled is true unless agentMCP is explicitly false (default on).
func (c *Config) AgentMCPEnabled() bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AgentMCP == nil {
		return true
	}
	return *c.AgentMCP
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

// legacyRoleGrantProjects returns the names of projects in raw config JSON that
// still use the removed allowedRoleIds / capabilityByRole keys, sorted.
//
// It reads the raw bytes because the parsed Config no longer has those fields —
// there is nothing left to inspect after json.Unmarshal.
//
// Roles are deliberately NOT expanded to members: guild role membership needs a
// Discord call this process may not be able to make, and guessing would either
// grant the wrong people or fail at a moment nobody is watching.
func legacyRoleGrantProjects(raw []byte) []string {
	var top struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil // the real parse already reported this
	}
	var out []string
	for name, rb := range top.Projects {
		var legacy struct {
			AllowedRoleIDs   []string          `json:"allowedRoleIds"`
			CapabilityByRole map[string]string `json:"capabilityByRole"`
		}
		// A string-form entry ("app": "/path") fails to decode into a struct and
		// simply has no role keys to warn about.
		if err := json.Unmarshal(rb, &legacy); err != nil {
			continue
		}
		if len(legacy.AllowedRoleIDs) > 0 || len(legacy.CapabilityByRole) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// legacyGitHubIdentityIDs returns the Discord ids that still carry a hand-written
// discordUserGitHub mapping, sorted.
//
// Like legacyRoleGrantProjects this reads the raw bytes, because the parsed
// Config no longer has the field to inspect. It reports the KEYS rather than a
// count: the whole point of the warning is that the operator has to go and tell
// those specific people to link their GitHub account, and "3 mappings dropped"
// is not something anyone can act on. The GitHub logins are deliberately not
// echoed — they were an admin's guess at somebody's handle, and repeating an
// unproven claim is what this change exists to stop.
func legacyGitHubIdentityIDs(raw []byte) []string {
	var top struct {
		DiscordUserGitHub map[string]json.RawMessage `json:"discordUserGitHub"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil // the real parse already reported this
	}
	return slices.Sorted(maps.Keys(top.DiscordUserGitHub))
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

	// Global storage has no UnmarshalJSON hook (projects normalize via
	// ProjectsMap). Normalize here or any save that round-trips outObj would
	// persist a bad block, and EffectiveStorage would see unvalidated fields.
	st, err := normalizeStorage(c.Storage, false)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	c.Storage = st

	// allowedRoleIds / capabilityByRole no longer exist as fields, so the parse
	// above silently dropped them. Say so loudly: a project whose only grant was
	// a Discord role is now fail-closed, and that has to be explained rather than
	// discovered when somebody cannot @Grok.
	for _, name := range legacyRoleGrantProjects(raw) {
		fmt.Fprintf(os.Stderr,
			"[warn] project %q: allowedRoleIds / capabilityByRole are no longer supported and were IGNORED. "+
				"Discord-role authorization is replaced by per-project teams — add those people to "+
				"projects.%s.teams.<team>.members (namespaced actor ids, e.g. \"discord:123\") "+
				"or to allowedUserIds. Until then nobody gains access to %q from a Discord role.\n",
			name, name, name)
	}

	// discordUserGitHub is gone too, and its loss is silent in a nastier way: a
	// dropped grant stops somebody working, whereas dropped attribution just
	// stops appearing in commit trailers, which nobody notices for weeks. An
	// admin's guess at a handle is no longer accepted as identity — the person
	// proves it themselves by linking their GitHub login — so name the people
	// whose attribution just stopped, because this warning is the only signal
	// anyone gets.
	if ids := legacyGitHubIdentityIDs(raw); len(ids) > 0 {
		fmt.Fprintf(os.Stderr,
			"[warn] discordUserGitHub is no longer supported and was IGNORED for %d user(s): %s. "+
				"Commit trailers and \"@login\" mentions now come only from a GitHub login the person "+
				"linked themselves — ask each of them to sign in to the web UI and link GitHub at /account. "+
				"Until they do they simply get no attribution (no wrong trailer, no invented @login).\n",
			len(ids), strings.Join(ids, ", "))
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

	for _, name := range slices.Sorted(maps.Keys(c.Projects)) {
		pc := c.Projects[name]
		cwd := pc.Path
		if !filepath.IsAbs(cwd) {
			return nil, fmt.Errorf("project %q path must be absolute: %s", name, cwd)
		}
		if _, err := os.Stat(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] project %q path does not exist: %s\n", name, cwd)
		}
		if raw := strings.TrimSpace(pc.PrimaryBranch); raw != "" {
			if err := ValidatePrimaryBranchName(raw); err != nil {
				fmt.Fprintf(os.Stderr,
					"[warn] project %q: primaryBranch %q is invalid (%v) and will be ignored until fixed in Workflow settings\n",
					name, raw, err)
			}
		}
		// ClickUp customIdPrefix colliding with Linear teamKey: bare PREFIX-N stays
		// Linear-only; free-text ClickUp prefix parse is off. Warn so the silence
		// is not mistaken for a broken key.
		if pc.ClickUp != nil && pc.ClickUp.Enabled && pc.Linear != nil && pc.Linear.Enabled {
			prefix := strings.ToUpper(strings.TrimSpace(pc.ClickUp.CustomIdPrefix))
			team := strings.ToUpper(strings.TrimSpace(pc.Linear.TeamKey))
			if prefix != "" && prefix == team {
				fmt.Fprintf(os.Stderr,
					"[warn] project %q: clickup.customIdPrefix %q equals linear.teamKey — bare %s-N free-text binds as Linear only; "+
						"ClickUp prefix-parse is disabled. Use ClickUp URLs or change the prefix.\n",
					name, prefix, prefix)
			}
		}
		// A capability template that resolves to nothing is not a soft problem:
		// whoever it applies to gets zero capabilities (ResolveCapabilities fails
		// closed rather than promoting them to the unmapped default).
		for _, ref := range unresolvedTemplateRefs(pc) {
			fmt.Fprintf(os.Stderr,
				"[warn] project %q: %s names no known capability template — "+
					"everyone it applies to gets NO capabilities until it is fixed "+
					"(known: builtin investigator/operator/builder/approver/admin plus "+
					"projects.%s.capabilityTemplates).\n",
				name, ref, name)
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
	if c.ClaudeBin == "" {
		c.ClaudeBin = grokrun.AgentClaude.DefaultBin()
	}
	if c.CursorBin == "" {
		c.CursorBin = grokrun.AgentCursor.DefaultBin()
	}
	// Reject an unknown agent rather than silently running the default: a typo
	// would otherwise route every session to the wrong CLI.
	if _, ok := grokrun.ParseAgent(c.Agent); !ok {
		return nil, fmt.Errorf("agent %q is not a known coding CLI (want %s)",
			c.Agent, grokrun.KnownAgents())
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
		DiscordToken              string               `json:"discordToken"`
		DiscordClientID           string               `json:"discordClientId,omitempty"`
		DiscordClientSecret       string               `json:"discordClientSecret,omitempty"`
		Projects                  ProjectsMap          `json:"projects"`
		Channels                  map[string]string    `json:"channels"`
		GrokBin                   string               `json:"grokBin"`
		Agent                     string               `json:"agent,omitempty"`
		ClaudeBin                 string               `json:"claudeBin,omitempty"`
		CursorBin                 string               `json:"cursorBin,omitempty"`
		SummarizeModel            string               `json:"summarizeModel,omitempty"`
		ReviewModel               string               `json:"reviewModel,omitempty"`
		ModelRates                map[string]ModelRate `json:"modelRates,omitempty"`
		ClaudeExtraArgs           []string             `json:"claudeExtraArgs,omitempty"`
		ClaudeIncludeAnthropicEnv *bool                `json:"claudeIncludeAnthropicEnv,omitempty"`
		Yolo                      *bool                `json:"yolo"`
		Model                     string               `json:"model"`
		MaxTurns                  int                  `json:"maxTurns"`
		TimeoutMs                 int                  `json:"timeoutMs"`
		ExtraArgs                 []string             `json:"extraArgs"`
		SummarizeThreadTitle      *bool                `json:"summarizeThreadTitle"`
		SummarizeTimeoutMs        int                  `json:"summarizeTimeoutMs"`
		WorktreeIsolation         *bool                `json:"worktreeIsolation"`
		WorktreeDir               string               `json:"worktreeDir,omitempty"`
		WorktreeIdleTTLDays       *int                 `json:"worktreeIdleTTLDays,omitempty"`
		TerminalSessionTTLDays    *int                 `json:"terminalSessionTTLDays,omitempty"`
		HTTPListen                string               `json:"httpListen,omitempty"`
		WebPublicBaseURL          string               `json:"webPublicBaseURL,omitempty"`
		DiscordGuildID            string               `json:"discordGuildId,omitempty"`
		WebMergeMethod            string               `json:"webMergeMethod,omitempty"`
		DiscordPRLink             string               `json:"discordPRLink,omitempty"`
		WebAuth                   *WebAuthConfig       `json:"webAuth,omitempty"`
		API                       *APIConfig           `json:"api,omitempty"`
		RiskyPathGlobs            []string             `json:"riskyPathGlobs,omitempty"`
		AutoFixCI                 *bool                `json:"autoFixCI,omitempty"`
		AgentMCP                  *bool                `json:"agentMCP,omitempty"`
		AutoFixCIMax              int                  `json:"autoFixCIMax,omitempty"`
		BoardStaleDays            *int                 `json:"boardStaleDays,omitempty"`
		BoardDigestChannel        string               `json:"boardDigestChannel,omitempty"`
		ResumeActiveRuns          *bool                `json:"resumeActiveRuns,omitempty"`
		ShutdownTimeoutMs         int                  `json:"shutdownTimeoutMs,omitempty"`
		MaxConcurrentRuns         *int                 `json:"maxConcurrentRuns,omitempty"`
		MaxConcurrentRunsUser     *int                 `json:"maxConcurrentRunsUser,omitempty"`
		GrokEnvDenylist           []string             `json:"grokEnvDenylist,omitempty"`
		NotifyOnDone              string               `json:"notifyOnDone,omitempty"`
		NotifyOnDoneLongMs        int                  `json:"notifyOnDoneLongMs,omitempty"`
		MaxConcurrentDeploys      *int                 `json:"maxConcurrentDeploys,omitempty"`
		DeployRunRetention        *int                 `json:"deployRunRetention,omitempty"`
		Storage                   *StorageConfig       `json:"storage,omitempty"`
	}{
		DiscordToken:              c.DiscordToken,
		DiscordClientID:           c.DiscordClientID,
		DiscordClientSecret:       c.DiscordClientSecret,
		Projects:                  cloneProjectsMap(c.Projects),
		Channels:                  cloneStringMap(c.Channels),
		GrokBin:                   c.GrokBin,
		Agent:                     c.Agent,
		ClaudeBin:                 c.ClaudeBin,
		CursorBin:                 c.CursorBin,
		SummarizeModel:            c.SummarizeModel,
		ReviewModel:               c.ReviewModel,
		ModelRates:                cloneModelRates(c.ModelRates),
		ClaudeExtraArgs:           slices.Clone(c.ClaudeExtraArgs),
		ClaudeIncludeAnthropicEnv: cloneBoolPtr(c.ClaudeIncludeAnthropicEnv),
		Yolo:                      c.Yolo,
		Model:                     c.Model,
		MaxTurns:                  c.MaxTurns,
		TimeoutMs:                 c.TimeoutMs,
		ExtraArgs:                 slices.Clone(c.ExtraArgs),
		SummarizeThreadTitle:      c.SummarizeThreadTitle,
		SummarizeTimeoutMs:        c.SummarizeTimeoutMs,
		WorktreeIsolation:         c.WorktreeIsolation,
		WorktreeDir:               strings.TrimSpace(c.WorktreeDir),
		WorktreeIdleTTLDays:       cloneIntPtr(c.WorktreeIdleTTLDays),
		TerminalSessionTTLDays:    cloneIntPtr(c.TerminalSessionTTLDays),
		HTTPListen:                c.HTTPListen,
		WebPublicBaseURL:          c.WebPublicBaseURL,
		DiscordGuildID:            c.DiscordGuildID,
		WebMergeMethod:            c.WebMergeMethod,
		DiscordPRLink:             c.DiscordPRLink,
		WebAuth:                   cloneWebAuth(c.WebAuth),
		API:                       cloneAPI(c.API),
		RiskyPathGlobs:            slices.Clone(c.RiskyPathGlobs),
		AutoFixCI:                 c.AutoFixCI,
		AgentMCP:                  cloneBoolPtr(c.AgentMCP),
		AutoFixCIMax:              c.AutoFixCIMax,
		BoardStaleDays:            cloneIntPtr(c.BoardStaleDays),
		BoardDigestChannel:        c.BoardDigestChannel,
		ResumeActiveRuns:          cloneBoolPtr(c.ResumeActiveRuns),
		ShutdownTimeoutMs:         c.ShutdownTimeoutMs,
		MaxConcurrentRuns:         cloneIntPtr(c.MaxConcurrentRuns),
		MaxConcurrentRunsUser:     cloneIntPtr(c.MaxConcurrentRunsUser),
		GrokEnvDenylist:           slices.Clone(c.GrokEnvDenylist),
		NotifyOnDone:              c.NotifyOnDone,
		NotifyOnDoneLongMs:        c.NotifyOnDoneLongMs,
		MaxConcurrentDeploys:      cloneIntPtr(c.MaxConcurrentDeploys),
		DeployRunRetention:        cloneIntPtr(c.DeployRunRetention),
		Storage:                   cloneStorage(c.Storage),
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// config.json holds every project, allowlist, channel map and credential
	// the bot has: it is replaced durably, never written in place. See
	// atomicfile.Write for why (including the directory fsync nobody expects).
	return atomicfile.Write(c.ConfigPath, raw, 0o600)
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

// SetConcurrencyLimits sets the host-wide and per-user active-run caps and
// persists. Either may be nil (unlimited); a non-nil value must be >= 0 (0
// also means unlimited — see MaxConcurrentRunsValue / MaxConcurrentRunsUserValue).
// Callers that treat an empty/0 form input as "unlimited" should pass nil
// rather than a pointer to 0, so config.json stays free of the field.
func (c *Config) SetConcurrencyLimits(maxConcurrentRuns, maxConcurrentRunsUser *int) error {
	if maxConcurrentRuns != nil && *maxConcurrentRuns < 0 {
		return fmt.Errorf("maxConcurrentRuns must be >= 0")
	}
	if maxConcurrentRunsUser != nil && *maxConcurrentRunsUser < 0 {
		return fmt.Errorf("maxConcurrentRunsUser must be >= 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MaxConcurrentRuns = cloneIntPtr(maxConcurrentRuns)
	c.MaxConcurrentRunsUser = cloneIntPtr(maxConcurrentRunsUser)
	return c.saveLocked()
}

// WorktreesRoot returns the directory that contains <project>/<unitID> worktrees.
// Empty WorktreeDir → <DataDir>/worktrees. Relative WorktreeDir is resolved
// against the config file directory.
func (c *Config) WorktreesRoot() string {
	if c == nil {
		return "worktrees"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.worktreesRootLocked()
}

func (c *Config) worktreesRootLocked() string {
	raw := strings.TrimSpace(c.WorktreeDir)
	if raw == "" {
		if c.DataDir == "" {
			return "worktrees"
		}
		return filepath.Join(c.DataDir, "worktrees")
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	base := ""
	if c.ConfigPath != "" {
		base = filepath.Dir(c.ConfigPath)
	}
	if base == "" {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(base, raw))
}

// WorktreeDirValue returns the configured worktreeDir override (may be empty).
func (c *Config) WorktreeDirValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.WorktreeDir)
}

// SetWorktreeDir sets the worktree root for new worktrees and persists.
// Empty clears the override (use DataDir/worktrees). Relative paths are stored
// as given and resolved at runtime against the config directory.
func (c *Config) SetWorktreeDir(dir string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	dir = strings.TrimSpace(dir)
	// Reject control characters / NULs that would never be valid paths.
	if strings.ContainsRune(dir, 0) {
		return fmt.Errorf("worktreeDir must not contain NUL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorktreeDir = dir
	return c.saveLocked()
}

// SetWorktreeSettings sets worktree idle TTL, terminal session TTL, and worktree
// root together and persists. Either TTL may be 0 to disable that cleanup.
// worktreeDir: empty clears override (DataDir/worktrees).
func (c *Config) SetWorktreeSettings(days int, worktreeDir string, terminalSessionDays int) error {
	if days < 0 {
		return fmt.Errorf("worktreeIdleTTLDays must be >= 0 (0 disables cleanup)")
	}
	if terminalSessionDays < 0 {
		return fmt.Errorf("terminalSessionTTLDays must be >= 0 (0 disables cleanup)")
	}
	worktreeDir = strings.TrimSpace(worktreeDir)
	if strings.ContainsRune(worktreeDir, 0) {
		return fmt.Errorf("worktreeDir must not contain NUL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := days
	c.WorktreeIdleTTLDays = &d
	td := terminalSessionDays
	c.TerminalSessionTTLDays = &td
	c.WorktreeDir = worktreeDir
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

// Discord PR link targets for Discord messages (cards, status, timeline, completion).
const (
	DiscordPRLinkGitHub = "github"
	DiscordPRLinkWeb    = "web"
)

// DiscordPRLinkValue returns github|web (default github).
func (c *Config) DiscordPRLinkValue() string {
	if c == nil {
		return DiscordPRLinkGitHub
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.discordPRLinkLocked()
}

func (c *Config) discordPRLinkLocked() string {
	switch strings.ToLower(strings.TrimSpace(c.DiscordPRLink)) {
	case DiscordPRLinkWeb:
		return DiscordPRLinkWeb
	default:
		return DiscordPRLinkGitHub
	}
}

// SetDiscordPRLink sets which URL is shown for PRs in Discord (github|web) and persists.
func (c *Config) SetDiscordPRLink(mode string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case DiscordPRLinkGitHub, "":
		mode = DiscordPRLinkGitHub
	case DiscordPRLinkWeb:
		mode = DiscordPRLinkWeb
	default:
		return fmt.Errorf("discordPRLink must be %q or %q", DiscordPRLinkGitHub, DiscordPRLinkWeb)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if mode == DiscordPRLinkGitHub {
		c.DiscordPRLink = "" // omit default from disk
	} else {
		c.DiscordPRLink = mode
	}
	return c.saveLocked()
}

// DiscordPRDisplayURL returns the URL to show in Discord for a PR.
// Stored/session URLs stay GitHub; this only rewrites for display when mode is web
// and webPublicBaseURL (or GROK_WORK_PUBLIC_BASE_URL) is set. Falls back to githubURL
// (or a constructed github.com URL) otherwise.
func (c *Config) DiscordPRDisplayURL(owner, repo string, number int, githubURL string) string {
	githubURL = strings.TrimSpace(githubURL)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" || number <= 0 {
		if m := githubPRURLRE.FindStringSubmatch(githubURL); len(m) >= 4 {
			owner, repo = m[1], m[2]
			if n, err := strconv.Atoi(m[3]); err == nil {
				number = n
			}
		}
	}
	if githubURL == "" && owner != "" && repo != "" && number > 0 {
		githubURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
	}
	if c == nil || c.DiscordPRLinkValue() != DiscordPRLinkWeb {
		return githubURL
	}
	if owner == "" || repo == "" || number <= 0 {
		return githubURL
	}
	base := c.WebPublicBaseURLValue()
	if base == "" {
		return githubURL
	}
	return fmt.Sprintf("%s/prs/%s/%s/%d", base, pathEscapeSegment(owner), pathEscapeSegment(repo), number)
}

// pathEscapeSegment escapes a path segment for use in /prs/{owner}/{repo}/{n}.
// Keep alphanumerics, hyphen, underscore, and dot unescaped (GitHub slug chars).
func pathEscapeSegment(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// githubPRURLRE matches https://github.com/owner/repo/pull/N for display URL resolution.
var githubPRURLRE = regexp.MustCompile(`(?i)https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/(\d+)`)

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
	for line := range strings.SplitSeq(text, "\n") {
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
		defaultTpl := strings.TrimSpace(pc.SafeTeamDefaultTemplate)
		if defaultTpl == "" {
			defaultTpl = "investigator"
		}
		item := ProjectItem{
			Name:                     n,
			Path:                     pc.Path,
			LinearEnvHint:            "LINEAR_API_KEY_" + ProjectEnvKeySuffix(n),
			DiscordChannelID:         strings.TrimSpace(pc.DiscordChannelID),
			DiscordGuildID:           strings.TrimSpace(pc.DiscordGuildID),
			AllowedUserIDs:           slices.Clone(pc.AllowedUserIDs),
			Teams:                    teamItems(pc),
			MemberIDs:                projectActorIDsLocked(pc),
			RepoFetchIntervalMinutes: fetchMins,
			DirectToPrimary:          pc.DirectToPrimary != nil && *pc.DirectToPrimary,
			AgentMCPAlways:           pc.AgentMCPAlways != nil && *pc.AgentMCPAlways,
			SafeTeamMode:             pc.SafeTeamMode != nil && *pc.SafeTeamMode,
			SafeTeamDefaultTemplate:  defaultTpl,
			DefaultMode:              strings.TrimSpace(strings.ToLower(pc.DefaultMode)),
			CaseKey:                  strings.TrimSpace(pc.CaseKey),
			PrimaryBranch:            strings.TrimSpace(pc.PrimaryBranch),
			PrimaryBranchInvalid: func() bool {
				raw := strings.TrimSpace(pc.PrimaryBranch)
				return raw != "" && ValidatePrimaryBranchName(raw) != nil
			}(),
			SLA:                     slaItems(pc.SLA),
			SLAConfigured:           slaConfigured(pc.SLA),
			CapabilityByUser:        capabilityMapItems(pc.CapabilityByUser),
			UnmappedUserIDs:         unmappedIDs(pc.AllowedUserIDs, pc.CapabilityByUser),
			CapabilityTemplateNames: capabilityTemplateNames(pc.CapabilityTemplates),
			VerifyCommandsText:      FormatVerifyCommandsText(pc.VerifyCommands),
		}
		item.DeployEnabled, item.DeployManifestPath, item.DeployEnvs = deployItems(pc.Deploy)
		if pc.Actions != nil && len(pc.Actions.DispatchRules) > 0 {
			item.ActionsRules = cloneProjectActions(pc.Actions).DispatchRules
		}
		item.StorageSource = c.storageSourceLocked(n)
		item.StorageDisabled = item.StorageSource == StorageSourceDisabled
		item.StorageInherited = item.StorageSource == StorageSourceGlobal
		if pc.Storage != nil && !pc.Storage.Disabled {
			item.StorageBackend = pc.Storage.Backend
			item.StorageBucket = pc.Storage.GCSBucket
			item.StoragePrefix = pc.Storage.Prefix
			item.StorageDriveFolderID = pc.Storage.DriveFolderID
			item.StorageCredentialsFile = pc.Storage.CredentialsFile
		}
		if eff := c.effectiveStorageLocked(n); eff != nil {
			item.StorageEffectiveBackend = eff.Backend
			item.StorageEffectiveBucket = eff.GCSBucket
			item.StorageEffectivePrefix = eff.Prefix
			item.StorageEffectiveDriveFolderID = eff.DriveFolderID
			item.StorageEffectiveIsolation = eff.IsolationSegment
		}
		if pc.Linear != nil {
			item.LinearEnabled = pc.Linear.Enabled
			item.LinearTeamKey = strings.TrimSpace(pc.Linear.TeamKey)
			item.LinearAPIKeySet = strings.TrimSpace(pc.Linear.APIKey) != "" || linearAPIKeyFromEnv(n) != ""
		} else if linearAPIKeyFromEnv(n) != "" {
			item.LinearAPIKeySet = true
		}
		item.ClickUpEnvHint = "CLICKUP_API_KEY_" + ProjectEnvKeySuffix(n)
		if pc.ClickUp != nil {
			item.ClickUpEnabled = pc.ClickUp.Enabled
			item.ClickUpWorkspaceID = strings.TrimSpace(pc.ClickUp.WorkspaceID)
			item.ClickUpListID = strings.TrimSpace(pc.ClickUp.ListID)
			item.ClickUpCustomIdPrefix = strings.ToUpper(strings.TrimSpace(pc.ClickUp.CustomIdPrefix))
			item.ClickUpAPIKeySet = strings.TrimSpace(pc.ClickUp.APIKey) != "" || clickupAPIKeyFromEnv(n) != ""
		} else if clickupAPIKeyFromEnv(n) != "" {
			item.ClickUpAPIKeySet = true
		}
		item.SentryEnvHint = "SENTRY_AUTH_TOKEN_" + ProjectEnvKeySuffix(n)
		item.DeploysEnvHint = "DEPLOYS_API_TOKEN_" + ProjectEnvKeySuffix(n) + " — not DEPLOYS_TOKEN"
		if pc.Errors != nil {
			if pc.Errors.GCP != nil {
				item.GCPErrorsEnabled = pc.Errors.GCP.Enabled
				item.GCPProjectID = strings.TrimSpace(pc.Errors.GCP.ProjectID)
				item.GCPProjectNumber = strings.TrimSpace(pc.Errors.GCP.ProjectNumber)
				item.GCPService = strings.TrimSpace(pc.Errors.GCP.Service)
				item.GCPCredentialsFile = strings.TrimSpace(pc.Errors.GCP.CredentialsFile)
			}
			if pc.Errors.Sentry != nil {
				item.SentryEnabled = pc.Errors.Sentry.Enabled
				item.SentryOrg = strings.TrimSpace(pc.Errors.Sentry.Org)
				item.SentryProject = strings.TrimSpace(pc.Errors.Sentry.Project)
				item.SentryBaseURL = strings.TrimSpace(pc.Errors.Sentry.BaseURL)
				item.SentryAuthTokenSet = strings.TrimSpace(pc.Errors.Sentry.AuthToken) != "" || sentryAuthTokenFromEnv(n) != ""
			} else if sentryAuthTokenFromEnv(n) != "" {
				item.SentryAuthTokenSet = true
			}
			if pc.Errors.Deploys != nil {
				item.DeploysErrorsEnabled = pc.Errors.Deploys.Enabled
				item.DeploysErrorsProject = strings.TrimSpace(pc.Errors.Deploys.Project)
				item.DeploysErrorsLocation = strings.TrimSpace(pc.Errors.Deploys.Location)
				item.DeploysErrorsName = strings.TrimSpace(pc.Errors.Deploys.Deployment)
				item.DeploysAPITokenSet = strings.TrimSpace(pc.Errors.Deploys.APIToken) != "" || deploysAPITokenFromEnv(n) != ""
				u, p := deploysBasicFromEnv(n)
				if u != "" && p != "" {
					item.DeploysAPITokenSet = true
				}
			} else if deploysAPITokenFromEnv(n) != "" {
				item.DeploysAPITokenSet = true
			} else if u, p := deploysBasicFromEnv(n); u != "" && p != "" {
				item.DeploysAPITokenSet = true
			}
		} else {
			if sentryAuthTokenFromEnv(n) != "" {
				item.SentryAuthTokenSet = true
			}
			if deploysAPITokenFromEnv(n) != "" {
				item.DeploysAPITokenSet = true
			} else if u, p := deploysBasicFromEnv(n); u != "" && p != "" {
				item.DeploysAPITokenSet = true
			}
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
	termDays := DefaultTerminalSessionTTLDays
	if c.TerminalSessionTTLDays != nil {
		termDays = *c.TerminalSessionTTLDays
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
	agent := c.defaultAgentLocked()
	rawFallback, _ := grokrun.ParseAgent(c.Agent)
	modelAgent, modelAgentKnown := grokrun.AgentForModel(c.Model)
	if !modelAgentKnown {
		modelAgent = agent
	}
	summarizeAgent, summarizeAgentKnown := grokrun.AgentForModel(c.SummarizeModel)
	if !summarizeAgentKnown {
		summarizeAgent = modelAgent
	}
	reviewAgent, reviewAgentKnown := grokrun.AgentForModel(c.ReviewModel)
	if !reviewAgentKnown {
		reviewAgent = modelAgent
	}
	claudeBin := strings.TrimSpace(c.ClaudeBin)
	if claudeBin == "" {
		claudeBin = grokrun.AgentClaude.DefaultBin()
	}
	cursorBin := strings.TrimSpace(c.CursorBin)
	if cursorBin == "" {
		cursorBin = grokrun.AgentCursor.DefaultBin()
	}
	rateItems := modelRateItemsFrom(c.ModelRates)
	ratesSet := 0
	for _, it := range rateItems {
		if it.Configured {
			ratesSet++
		}
	}
	snap := Snapshot{
		Projects:                  projects,
		Channels:                  channels,
		ProjectNames:              names,
		HTTPListen:                c.HTTPListen,
		GrokBin:                   c.GrokBin,
		Model:                     c.Model,
		Agent:                     agent.String(),
		ClaudeBin:                 claudeBin,
		CursorBin:                 cursorBin,
		SummarizeModel:            strings.TrimSpace(c.SummarizeModel),
		ReviewModel:               strings.TrimSpace(c.ReviewModel),
		ModelAgent:                modelAgent.String(),
		ModelAgentKnown:           modelAgentKnown,
		SummarizeAgent:            summarizeAgent.String(),
		SummarizeAgentKnown:       summarizeAgentKnown,
		ReviewAgent:               reviewAgent.String(),
		ReviewAgentKnown:          reviewAgentKnown,
		ModelLimits:               modelAgent.Limitations(),
		SummarizeLimits:           summarizeAgent.Limitations(),
		ReviewLimits:              reviewAgent.Limitations(),
		FallbackLimits:            rawFallback.Limitations(),
		ModelGroups:               ModelGroups(c.Model),
		SummarizeModelGroups:      ModelGroups(c.SummarizeModel),
		ReviewModelGroups:         ModelGroups(c.ReviewModel),
		ModelRates:                rateItems,
		ModelRatesSet:             ratesSet,
		ClaudeIncludeAnthropicEnv: c.ClaudeIncludeAnthropicEnv != nil && *c.ClaudeIncludeAnthropicEnv,
		MaxTurns:                  maxTurns,
		TimeoutMs:                 timeoutMs,
		MaxConcurrentRuns:         c.MaxConcurrentRunsValue(),
		MaxConcurrentRunsUser:     c.MaxConcurrentRunsUserValue(),
		Yolo:                      c.YoloEnabled(),
		WorktreeIsolation:         c.WorktreeIsolationEnabled(),
		WorktreeDir:               strings.TrimSpace(c.WorktreeDir),
		WorktreesRoot:             c.worktreesRootLocked(),
		WorktreeIdleTTLDays:       idleDays,
		TerminalSessionTTLDays:    termDays,
		AutoFixCI:                 c.AutoFixCI != nil && *c.AutoFixCI,
		AutoFixCIMax:              autoFixMax,
		RiskyPathGlobsText:        riskyText,
		RiskyPathUseDefault:       riskyDefault,
		BoardStaleDays:            boardStale,
		BoardDigestChannel:        strings.TrimSpace(c.BoardDigestChannel),
		NotifyOnDone:              notifyOnDoneEffectiveLocked(c.NotifyOnDone),
		NotifyOnDoneLongMs:        notifyOnDoneLongMsEffectiveLocked(c.NotifyOnDoneLongMs),
		ResumeActiveRuns:          c.ResumeActiveRuns == nil || *c.ResumeActiveRuns,
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
		DiscordPRLink:     c.discordPRLinkLocked(),
		// Snapshot holds RLock — resolve base URL without re-locking via local fields.
		WebPublicBaseURL: func() string {
			base := strings.TrimSpace(c.WebPublicBaseURL)
			if base != "" {
				return strings.TrimRight(base, "/")
			}
			// Env is process-wide; safe under RLock for a read-only Snapshot field.
			if v := EnvWork("PUBLIC_BASE_URL"); v != "" {
				return strings.TrimRight(v, "/")
			}
			return ""
		}(),
	}
	if c.Storage != nil {
		snap.StorageBackend = c.Storage.Backend
		snap.StorageBucket = c.Storage.GCSBucket
		snap.StoragePrefix = c.Storage.Prefix
		snap.StorageDriveFolderID = c.Storage.DriveFolderID
		snap.StorageCredentialsFile = c.Storage.CredentialsFile
		snap.StorageConfigured = storageHasIdentity(c.Storage)
	}
	// Features need WebAuthEnabled without re-locking — compute inline.
	if c.WebAuth != nil && c.WebAuth.Enabled {
		snap.FeatureGitHubWrites = c.WebAuth.Features.GitHubWrites
		snap.FeatureMerge = c.WebAuth.Features.Merge
		snap.FeaturePRReviews = c.WebAuth.Features.PRReviews
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

// teamItems renders a project's teams for the config UI, sorted by key.
// TemplateUnknown is resolved with the already-held lock's overlay map rather
// than ResolveTemplate, which would take c.mu.RLock again and deadlock.
// Caller holds c.mu.
func teamItems(pc ProjectConfig) []TeamItem {
	if len(pc.Teams) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(pc.Teams))
	out := make([]TeamItem, 0, len(keys))
	for _, k := range keys {
		t := pc.Teams[k]
		label := strings.TrimSpace(t.Label)
		if label == "" {
			label = k
		}
		item := TeamItem{
			Key:          k,
			Label:        label,
			Capabilities: t.Capabilities,
			Members:      slices.Clone(t.Members),
		}
		if t.Capabilities != "" {
			if _, ok := lookupTemplate(t.Capabilities, pc.CapabilityTemplates); !ok {
				item.TemplateUnknown = true
			}
		}
		out = append(out, item)
	}
	return out
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

func capabilityMapItems(m map[string]string) []CapabilityMapItem {
	if len(m) == 0 {
		return nil
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]CapabilityMapItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, CapabilityMapItem{ID: id, Template: m[id]})
	}
	return out
}

func unmappedIDs(allowed []string, mapped map[string]string) []string {
	if len(allowed) == 0 {
		return nil
	}
	var out []string
	for _, id := range allowed {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if mapped == nil {
			out = append(out, id)
			continue
		}
		if _, ok := mapped[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func capabilityTemplateNames(overlays map[string]Capabilities) []string {
	seen := make(map[string]struct{})
	var names []string
	for name := range BuiltinCapabilityTemplates {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for name := range overlays {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
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
