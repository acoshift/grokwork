package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ProjectLinearConfig is per-project Linear integration settings (opt-in).
type ProjectLinearConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`  // secret; never log or send to Discord
	TeamKey string `json:"teamKey,omitempty"` // e.g. "ENG"
}

// ProjectConfig is one named project entry (path + optional integrations).
type ProjectConfig struct {
	Path             string               `json:"path"`
	DiscordChannelID string               `json:"discordChannelId,omitempty"` // preferred channel for web-created threads
	DiscordGuildID   string               `json:"discordGuildId,omitempty"`   // Discord server for deep links (multi-guild)
	// AllowedUserIDs / AllowedRoleIDs are this project's Discord allowlist.
	// Empty both → fail-closed (no one may @Grok on this project's channels).
	AllowedUserIDs []string             `json:"allowedUserIds,omitempty"`
	AllowedRoleIDs []string             `json:"allowedRoleIds,omitempty"`
	// RepoFetchIntervalMinutes controls idle background git fetch for this
	// project's main checkout. nil/omitted → DefaultRepoFetchIntervalMinutes (5).
	// 0 disables idle auto-fetch. New worktrees always fetch with a short
	// hardcoded throttle (see gitworktree.CreateFetchThrottle).
	RepoFetchIntervalMinutes *int                 `json:"repoFetchIntervalMinutes,omitempty"`
	// DirectToPrimary, when true, new sessions stamp ShipMode=direct and ship
	// via fast-forward push to the project primary (no PR). nil/false = PR mode.
	DirectToPrimary *bool                 `json:"directToPrimary,omitempty"`
	// SafeTeamMode enables capability templates (K16). nil/false → legacy builder default.
	SafeTeamMode            *bool                    `json:"safeTeamMode,omitempty"`
	SafeTeamDefaultTemplate string                   `json:"safeTeamDefaultTemplate,omitempty"` // default investigator
	DefaultMode             string                   `json:"defaultMode,omitempty"`             // investigate|fix|… empty=legacy
	CapabilityTemplates     map[string]Capabilities  `json:"capabilityTemplates,omitempty"`
	CapabilityByRole        map[string]string        `json:"capabilityByRole,omitempty"` // Discord role ID → template
	CapabilityByUser        map[string]string        `json:"capabilityByUser,omitempty"` // Discord user ID → template
	InvestigateTools        string                   `json:"investigateTools,omitempty"` // comma tools allowlist
	// VerifyCommands are project shell checks run by @Grok /verify (no Grok).
	VerifyCommands []VerifyCommand `json:"verifyCommands,omitempty"`
	GitHub         *ProjectGitHubConfig `json:"github,omitempty"`
	Linear         *ProjectLinearConfig `json:"linear,omitempty"`
}

// VerifyCommand is one named verify harness entry.
type VerifyCommand struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs,omitempty"` // default 600000
}

// ProjectsMap is project name → config. JSON accepts either a path string or a full object.
type ProjectsMap map[string]ProjectConfig

// PathProjects builds a ProjectsMap from name→path only (tests and simple setups).
func PathProjects(m map[string]string) ProjectsMap {
	if m == nil {
		return ProjectsMap{}
	}
	out := make(ProjectsMap, len(m))
	for name, path := range m {
		out[name] = ProjectConfig{Path: path}
	}
	return out
}

func (m *ProjectsMap) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*m = nil
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(ProjectsMap, len(raw))
	for name, rb := range raw {
		var path string
		if err := json.Unmarshal(rb, &path); err == nil {
			out[name] = ProjectConfig{Path: path}
			continue
		}
		var pc ProjectConfig
		if err := json.Unmarshal(rb, &pc); err != nil {
			return fmt.Errorf("projects[%q]: %w", name, err)
		}
		if strings.TrimSpace(pc.Path) == "" {
			return fmt.Errorf("projects[%q]: path is required", name)
		}
		pc.Path = strings.TrimSpace(pc.Path)
		pc.DiscordChannelID = strings.TrimSpace(pc.DiscordChannelID)
		pc.DiscordGuildID = strings.TrimSpace(pc.DiscordGuildID)
		pc.AllowedUserIDs = cleanIDList(pc.AllowedUserIDs)
		pc.AllowedRoleIDs = cleanIDList(pc.AllowedRoleIDs)
		if pc.Linear != nil {
			pc.Linear.TeamKey = strings.TrimSpace(pc.Linear.TeamKey)
			pc.Linear.APIKey = strings.TrimSpace(pc.Linear.APIKey)
		}
		if pc.GitHub != nil {
			for i := range pc.GitHub.Repos {
				pc.GitHub.Repos[i].Owner = strings.TrimSpace(pc.GitHub.Repos[i].Owner)
				pc.GitHub.Repos[i].Repo = strings.TrimSpace(pc.GitHub.Repos[i].Repo)
			}
			pc.GitHub.Owner = strings.TrimSpace(pc.GitHub.Owner)
			pc.GitHub.Repo = strings.TrimSpace(pc.GitHub.Repo)
			if len(pc.GitHub.NormalizedRepos()) == 0 {
				pc.GitHub = nil
			}
		}
		out[name] = pc
	}
	*m = out
	return nil
}

func (m ProjectsMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	// Always write object form so Linear/GitHub fields round-trip.
	type outObj struct {
		Path                     string                  `json:"path"`
		DiscordChannelID         string                  `json:"discordChannelId,omitempty"`
		DiscordGuildID           string                  `json:"discordGuildId,omitempty"`
		AllowedUserIDs           []string                `json:"allowedUserIds,omitempty"`
		AllowedRoleIDs           []string                `json:"allowedRoleIds,omitempty"`
		RepoFetchIntervalMinutes *int                    `json:"repoFetchIntervalMinutes,omitempty"`
		DirectToPrimary          *bool                   `json:"directToPrimary,omitempty"`
		SafeTeamMode             *bool                   `json:"safeTeamMode,omitempty"`
		SafeTeamDefaultTemplate  string                  `json:"safeTeamDefaultTemplate,omitempty"`
		DefaultMode              string                  `json:"defaultMode,omitempty"`
		CapabilityTemplates      map[string]Capabilities `json:"capabilityTemplates,omitempty"`
		CapabilityByRole         map[string]string       `json:"capabilityByRole,omitempty"`
		CapabilityByUser         map[string]string       `json:"capabilityByUser,omitempty"`
		InvestigateTools         string                  `json:"investigateTools,omitempty"`
		VerifyCommands           []VerifyCommand         `json:"verifyCommands,omitempty"`
		GitHub                   *ProjectGitHubConfig    `json:"github,omitempty"`
		Linear                   *ProjectLinearConfig    `json:"linear,omitempty"`
	}
	out := make(map[string]outObj, len(m))
	for name, pc := range m {
		out[name] = outObj{
			Path:                     pc.Path,
			DiscordChannelID:         pc.DiscordChannelID,
			DiscordGuildID:           pc.DiscordGuildID,
			AllowedUserIDs:           slices.Clone(pc.AllowedUserIDs),
			AllowedRoleIDs:           slices.Clone(pc.AllowedRoleIDs),
			RepoFetchIntervalMinutes: cloneIntPtr(pc.RepoFetchIntervalMinutes),
			DirectToPrimary:          cloneBoolPtr(pc.DirectToPrimary),
			SafeTeamMode:             cloneBoolPtr(pc.SafeTeamMode),
			SafeTeamDefaultTemplate:  pc.SafeTeamDefaultTemplate,
			DefaultMode:              pc.DefaultMode,
			CapabilityTemplates:      cloneCapabilitiesMap(pc.CapabilityTemplates),
			CapabilityByRole:         cloneStringMap(pc.CapabilityByRole),
			CapabilityByUser:         cloneStringMap(pc.CapabilityByUser),
			InvestigateTools:         pc.InvestigateTools,
			VerifyCommands:           cloneVerifyCommands(pc.VerifyCommands),
			GitHub:                   cloneProjectGitHub(pc.GitHub),
			Linear:                   cloneProjectLinear(pc.Linear),
		}
	}
	return json.Marshal(out)
}

func cloneVerifyCommands(in []VerifyCommand) []VerifyCommand {
	if len(in) == 0 {
		return nil
	}
	return slices.Clone(in)
}

func cloneProjectLinear(l *ProjectLinearConfig) *ProjectLinearConfig {
	if l == nil {
		return nil
	}
	cp := *l
	return &cp
}

func cloneProjectsMap(m ProjectsMap) ProjectsMap {
	if m == nil {
		return ProjectsMap{}
	}
	out := make(ProjectsMap, len(m))
	for k, v := range m {
		out[k] = ProjectConfig{
			Path:                     v.Path,
			DiscordChannelID:         v.DiscordChannelID,
			DiscordGuildID:           v.DiscordGuildID,
			AllowedUserIDs:           slices.Clone(v.AllowedUserIDs),
			AllowedRoleIDs:           slices.Clone(v.AllowedRoleIDs),
			RepoFetchIntervalMinutes: cloneIntPtr(v.RepoFetchIntervalMinutes),
			DirectToPrimary:          cloneBoolPtr(v.DirectToPrimary),
			SafeTeamMode:             cloneBoolPtr(v.SafeTeamMode),
			SafeTeamDefaultTemplate:  v.SafeTeamDefaultTemplate,
			DefaultMode:              v.DefaultMode,
			CapabilityTemplates:      cloneCapabilitiesMap(v.CapabilityTemplates),
			CapabilityByRole:         cloneStringMap(v.CapabilityByRole),
			CapabilityByUser:         cloneStringMap(v.CapabilityByUser),
			InvestigateTools:         v.InvestigateTools,
			VerifyCommands:           cloneVerifyCommands(v.VerifyCommands),
			GitHub:                   cloneProjectGitHub(v.GitHub),
			Linear:                   cloneProjectLinear(v.Linear),
		}
	}
	return out
}

// ProjectVerifyCommands returns a copy of projects.*.verifyCommands.
func (c *Config) ProjectVerifyCommands(name string) []VerifyCommand {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok {
		return nil
	}
	return cloneVerifyCommands(pc.VerifyCommands)
}

// SetProjectVerifyCommands replaces verify commands and persists.
// Each entry needs non-empty name and command. Empty list clears.
func (c *Config) SetProjectVerifyCommands(name string, cmds []VerifyCommand) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	clean := make([]VerifyCommand, 0, len(cmds))
	for _, cmd := range cmds {
		n := strings.TrimSpace(cmd.Name)
		co := strings.TrimSpace(cmd.Command)
		if n == "" || co == "" {
			return fmt.Errorf("verify command requires name and command")
		}
		to := cmd.TimeoutMs
		if to < 0 {
			return fmt.Errorf("timeoutMs must be >= 0")
		}
		clean = append(clean, VerifyCommand{Name: n, Command: co, TimeoutMs: to})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if len(clean) == 0 {
		pc.VerifyCommands = nil
	} else {
		pc.VerifyCommands = clean
	}
	c.Projects[name] = pc
	return c.saveLocked()
}

// FormatVerifyCommandsText renders commands for the project config textarea.
// Each line: "name | command" or "name | command | timeoutMs".
func FormatVerifyCommandsText(cmds []VerifyCommand) string {
	if len(cmds) == 0 {
		return ""
	}
	var b strings.Builder
	for i, cmd := range cmds {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(cmd.Name)
		b.WriteString(" | ")
		b.WriteString(cmd.Command)
		if cmd.TimeoutMs > 0 {
			b.WriteString(" | ")
			b.WriteString(strconv.Itoa(cmd.TimeoutMs))
		}
	}
	return b.String()
}

// ParseVerifyCommandsText parses the project config textarea format.
// Lines: "name | command" or "name | command | timeoutMs".
// Also accepts "name: command" for simple entries (no timeout).
// Blank lines and lines starting with # are ignored.
func ParseVerifyCommandsText(text string) ([]VerifyCommand, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []VerifyCommand
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, command, timeoutStr string
		if strings.Contains(line, "|") {
			parts := strings.SplitN(line, "|", 3)
			name = strings.TrimSpace(parts[0])
			if len(parts) < 2 {
				return nil, fmt.Errorf("line %d: expected name | command", i+1)
			}
			command = strings.TrimSpace(parts[1])
			if len(parts) == 3 {
				timeoutStr = strings.TrimSpace(parts[2])
			}
		} else if idx := strings.Index(line, ":"); idx > 0 {
			name = strings.TrimSpace(line[:idx])
			command = strings.TrimSpace(line[idx+1:])
		} else {
			return nil, fmt.Errorf("line %d: use \"name | command\" or \"name: command\"", i+1)
		}
		if name == "" || command == "" {
			return nil, fmt.Errorf("line %d: name and command are required", i+1)
		}
		cmd := VerifyCommand{Name: name, Command: command}
		if timeoutStr != "" {
			ms, err := strconv.Atoi(timeoutStr)
			if err != nil || ms < 0 {
				return nil, fmt.Errorf("line %d: timeoutMs must be a non-negative integer", i+1)
			}
			cmd.TimeoutMs = ms
		}
		out = append(out, cmd)
	}
	return out, nil
}

// ProjectRepoFetchIntervalMinutes returns the effective idle auto-fetch
// interval in minutes for a project. Unknown/empty project uses the default.
// Explicit 0 disables idle auto-fetch.
func (c *Config) ProjectRepoFetchIntervalMinutes(name string) int {
	if c == nil {
		return DefaultRepoFetchIntervalMinutes
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.RepoFetchIntervalMinutes == nil {
		return DefaultRepoFetchIntervalMinutes
	}
	if *pc.RepoFetchIntervalMinutes < 0 {
		return DefaultRepoFetchIntervalMinutes
	}
	return *pc.RepoFetchIntervalMinutes
}

// ProjectRepoFetchInterval returns the idle auto-fetch throttle as a duration.
// 0 means idle auto-fetch is disabled for this project.
func (c *Config) ProjectRepoFetchInterval(name string) time.Duration {
	mins := c.ProjectRepoFetchIntervalMinutes(name)
	if mins <= 0 {
		return 0
	}
	return time.Duration(mins) * time.Minute
}

// IdleRepoFetchTarget is one project main checkout for the idle fetch loop.
type IdleRepoFetchTarget struct {
	Name     string
	Path     string
	Interval time.Duration // 0 = disabled
}

// IdleRepoFetchTargets returns projects with their idle-fetch intervals.
// Paths are unique by absolute path when possible so shared checkouts fetch once.
func (c *Config) IdleRepoFetchTargets() []IdleRepoFetchTarget {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	slices.Sort(names)
	seen := make(map[string]struct{}, len(names))
	out := make([]IdleRepoFetchTarget, 0, len(names))
	for _, n := range names {
		pc := c.Projects[n]
		path := strings.TrimSpace(pc.Path)
		if path == "" {
			continue
		}
		mins := DefaultRepoFetchIntervalMinutes
		if pc.RepoFetchIntervalMinutes != nil {
			if *pc.RepoFetchIntervalMinutes < 0 {
				mins = DefaultRepoFetchIntervalMinutes
			} else {
				mins = *pc.RepoFetchIntervalMinutes
			}
		}
		var interval time.Duration
		if mins > 0 {
			interval = time.Duration(mins) * time.Minute
		}
		key := path
		if interval <= 0 {
			// Still list disabled projects so callers can count them, but skip
			// path dedupe so another project sharing the path can stay enabled.
			out = append(out, IdleRepoFetchTarget{Name: n, Path: path, Interval: 0})
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, IdleRepoFetchTarget{Name: n, Path: path, Interval: interval})
	}
	return out
}

// SetProjectRepoFetchIntervalMinutes sets the per-project idle auto-fetch
// interval and persists. 0 disables idle auto-fetch. Negative values are rejected.
func (c *Config) SetProjectRepoFetchIntervalMinutes(name string, minutes int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if minutes < 0 {
		return fmt.Errorf("repoFetchIntervalMinutes must be >= 0 (0 disables idle auto-fetch)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	d := minutes
	pc.RepoFetchIntervalMinutes = &d
	c.Projects[name] = pc
	return c.saveLocked()
}

// ProjectDirectToPrimary reports whether new sessions for this project use
// direct-to-primary ship mode (no PR). nil/false → false (PR mode).
func (c *Config) ProjectDirectToPrimary(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.DirectToPrimary != nil && *pc.DirectToPrimary
}

// SetProjectDirectToPrimary sets per-project direct-to-primary mode and persists.
// true enables No-PR ship; false clears the opt-in (PR mode).
func (c *Config) SetProjectDirectToPrimary(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if enabled {
		v := true
		pc.DirectToPrimary = &v
	} else {
		pc.DirectToPrimary = nil
	}
	c.Projects[name] = pc
	return c.saveLocked()
}

// ValidDefaultModes are accepted projects.*.defaultMode values (empty = legacy fix).
var ValidDefaultModes = map[string]bool{
	"":            true,
	"investigate": true,
	"fix":         true,
	"explain":     true,
	"case":        true,
}

// SetProjectSafeTeam sets SafeTeamMode, default template, and defaultMode and persists.
// enabled=false clears SafeTeamMode (legacy builder default for unmapped).
// defaultTemplate empty → "investigator". defaultMode must be empty or a known mode.
func (c *Config) SetProjectSafeTeam(name string, enabled bool, defaultTemplate, defaultMode string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	defaultTemplate = strings.TrimSpace(strings.ToLower(defaultTemplate))
	if defaultTemplate == "" {
		defaultTemplate = "investigator"
	}
	defaultMode = strings.TrimSpace(strings.ToLower(defaultMode))
	if !ValidDefaultModes[defaultMode] {
		return fmt.Errorf("defaultMode must be empty, investigate, fix, explain, or case")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	// Reject unknown templates (builtin or project overlay).
	if _, ok := lookupTemplate(defaultTemplate, pc.CapabilityTemplates); !ok {
		return fmt.Errorf("unknown capability template %q", defaultTemplate)
	}
	if enabled {
		v := true
		pc.SafeTeamMode = &v
	} else {
		pc.SafeTeamMode = nil
	}
	if defaultTemplate == "investigator" {
		pc.SafeTeamDefaultTemplate = ""
	} else {
		pc.SafeTeamDefaultTemplate = defaultTemplate
	}
	pc.DefaultMode = defaultMode
	c.Projects[name] = pc
	return c.saveLocked()
}

// SetProjectCapabilityByUser sets or clears a user → template map entry.
// template empty removes the mapping.
func (c *Config) SetProjectCapabilityByUser(name, userID, template string) error {
	return c.setProjectCapabilityMap(name, true, userID, template)
}

// SetProjectCapabilityByRole sets or clears a role → template map entry.
// template empty removes the mapping.
func (c *Config) SetProjectCapabilityByRole(name, roleID, template string) error {
	return c.setProjectCapabilityMap(name, false, roleID, template)
}

// RemoveProjectCapabilityByUser removes a user capability map entry.
func (c *Config) RemoveProjectCapabilityByUser(name, userID string) error {
	return c.setProjectCapabilityMap(name, true, userID, "")
}

// RemoveProjectCapabilityByRole removes a role capability map entry.
func (c *Config) RemoveProjectCapabilityByRole(name, roleID string) error {
	return c.setProjectCapabilityMap(name, false, roleID, "")
}

func (c *Config) setProjectCapabilityMap(name string, byUser bool, id, template string) error {
	name = strings.TrimSpace(name)
	id = strings.TrimSpace(id)
	template = strings.TrimSpace(strings.ToLower(template))
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if id == "" {
		if byUser {
			return fmt.Errorf("user id is required")
		}
		return fmt.Errorf("role id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if template != "" {
		if _, ok := lookupTemplate(template, pc.CapabilityTemplates); !ok {
			return fmt.Errorf("unknown capability template %q", template)
		}
	}
	var m map[string]string
	if byUser {
		m = cloneStringMap(pc.CapabilityByUser)
	} else {
		m = cloneStringMap(pc.CapabilityByRole)
	}
	if template == "" {
		delete(m, id)
	} else {
		m[id] = template
	}
	if len(m) == 0 {
		m = nil
	}
	if byUser {
		pc.CapabilityByUser = m
	} else {
		pc.CapabilityByRole = m
	}
	c.Projects[name] = pc
	return c.saveLocked()
}

// ProjectEnvKeySuffix maps a project name to the env suffix for LINEAR_API_KEY_<SUFFIX>.
// Example: "homeconnect" → "HOMECONNECT", "hah-platform" → "HAH_PLATFORM".
func ProjectEnvKeySuffix(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(project))
	for _, r := range project {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToUpper(r))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func linearAPIKeyFromEnv(project string) string {
	suf := ProjectEnvKeySuffix(project)
	if suf == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("LINEAR_API_KEY_" + suf))
}

// ProjectLinearEnabled reports whether Linear is opted in for the project.
func (c *Config) ProjectLinearEnabled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.Linear != nil && pc.Linear.Enabled
}

// ProjectLinearAPIKey returns the effective API key for the project:
// config value if set, else LINEAR_API_KEY_<PROJECT> env.
func (c *Config) ProjectLinearAPIKey(name string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	var fromConfig string
	if pc, ok := c.Projects[name]; ok && pc.Linear != nil {
		fromConfig = strings.TrimSpace(pc.Linear.APIKey)
	}
	c.mu.RUnlock()
	if fromConfig != "" {
		return fromConfig
	}
	return linearAPIKeyFromEnv(name)
}

// ProjectLinearTeamKey returns the configured team key (e.g. "ENG"), or empty.
func (c *Config) ProjectLinearTeamKey(name string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Linear == nil {
		return ""
	}
	return strings.TrimSpace(pc.Linear.TeamKey)
}

// ProjectLinearCanResolve is true when Linear is enabled and an API key is available.
func (c *Config) ProjectLinearCanResolve(name string) bool {
	return c.ProjectLinearEnabled(name) && c.ProjectLinearAPIKey(name) != ""
}

// SetProjectLinear updates Linear settings for an existing project and persists.
// apiKey empty means leave the stored key unchanged; clearAPIKey true clears it.
func (c *Config) SetProjectLinear(name string, enabled bool, teamKey, apiKey string, clearAPIKey bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	teamKey = strings.TrimSpace(teamKey)
	apiKey = strings.TrimSpace(apiKey)

	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	lin := pc.Linear
	if lin == nil {
		lin = &ProjectLinearConfig{}
	} else {
		cp := *lin
		lin = &cp
	}
	lin.Enabled = enabled
	lin.TeamKey = teamKey
	if clearAPIKey {
		lin.APIKey = ""
	} else if apiKey != "" {
		lin.APIKey = apiKey
	}
	if !lin.Enabled && lin.APIKey == "" && lin.TeamKey == "" {
		pc.Linear = nil
	} else {
		pc.Linear = lin
	}
	c.Projects[name] = pc
	return c.saveLocked()
}

// AnyProjectLinearEnabled reports whether any project has Linear opted in.
func (c *Config) AnyProjectLinearEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, pc := range c.Projects {
		if pc.Linear != nil && pc.Linear.Enabled {
			return true
		}
	}
	return false
}
