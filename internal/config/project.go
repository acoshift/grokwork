package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
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
	GitHub         *ProjectGitHubConfig `json:"github,omitempty"`
	Linear         *ProjectLinearConfig `json:"linear,omitempty"`
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
		Path             string               `json:"path"`
		DiscordChannelID string               `json:"discordChannelId,omitempty"`
		DiscordGuildID   string               `json:"discordGuildId,omitempty"`
		AllowedUserIDs   []string             `json:"allowedUserIds,omitempty"`
		AllowedRoleIDs   []string             `json:"allowedRoleIds,omitempty"`
		GitHub           *ProjectGitHubConfig `json:"github,omitempty"`
		Linear           *ProjectLinearConfig `json:"linear,omitempty"`
	}
	out := make(map[string]outObj, len(m))
	for name, pc := range m {
		out[name] = outObj{
			Path:             pc.Path,
			DiscordChannelID: pc.DiscordChannelID,
			DiscordGuildID:   pc.DiscordGuildID,
			AllowedUserIDs:   slices.Clone(pc.AllowedUserIDs),
			AllowedRoleIDs:   slices.Clone(pc.AllowedRoleIDs),
			GitHub:           cloneProjectGitHub(pc.GitHub),
			Linear:           cloneProjectLinear(pc.Linear),
		}
	}
	return json.Marshal(out)
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
			Path:             v.Path,
			DiscordChannelID: v.DiscordChannelID,
			DiscordGuildID:   v.DiscordGuildID,
			AllowedUserIDs:   slices.Clone(v.AllowedUserIDs),
			AllowedRoleIDs:   slices.Clone(v.AllowedRoleIDs),
			GitHub:           cloneProjectGitHub(v.GitHub),
			Linear:           cloneProjectLinear(v.Linear),
		}
	}
	return out
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
