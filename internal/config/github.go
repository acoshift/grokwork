package config

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// GitHubRepoRef is owner/repo in a project catalog.
type GitHubRepoRef struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// Slug returns "owner/repo".
func (r GitHubRepoRef) Slug() string {
	return strings.TrimSpace(r.Owner) + "/" + strings.TrimSpace(r.Repo)
}

// Valid reports non-empty owner and repo.
func (r GitHubRepoRef) Valid() bool {
	return strings.TrimSpace(r.Owner) != "" && strings.TrimSpace(r.Repo) != ""
}

// ProjectGitHubConfig is optional multi-repo catalog for a project.
// Prefer Repos[]; legacy Owner/Repo is accepted as a single-entry catalog.
type ProjectGitHubConfig struct {
	Repos  []GitHubRepoRef `json:"repos,omitempty"`
	Owner string          `json:"owner,omitempty"`
	Repo  string          `json:"repo,omitempty"`
}

// NormalizedRepos returns configured repos (repos[] or legacy owner/repo).
func (g *ProjectGitHubConfig) NormalizedRepos() []GitHubRepoRef {
	if g == nil {
		return nil
	}
	var out []GitHubRepoRef
	seen := map[string]struct{}{}
	add := func(r GitHubRepoRef) {
		r.Owner = strings.TrimSpace(r.Owner)
		r.Repo = strings.TrimSpace(r.Repo)
		if !r.Valid() {
			return
		}
		key := strings.ToLower(r.Slug())
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	for _, r := range g.Repos {
		add(r)
	}
	if len(out) == 0 {
		add(GitHubRepoRef{Owner: g.Owner, Repo: g.Repo})
	}
	return out
}

func cloneProjectGitHub(g *ProjectGitHubConfig) *ProjectGitHubConfig {
	if g == nil {
		return nil
	}
	cp := *g
	if g.Repos != nil {
		cp.Repos = append([]GitHubRepoRef(nil), g.Repos...)
	}
	return &cp
}

// GitRunner runs a command (tests inject fakes for remote discovery).
type GitRunner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

var defaultGitRunner GitRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.Output()
}

// github remote URL patterns: nina.v@example.com:o/r.git, https://github.com/o/r.git
var (
	githubSSHRE   = regexp.MustCompile(`(?i)^(?:ssh://)?git@github\.com[:/]([^/\s]+)/([^/\s]+)$`)
	githubHTTPSRE = regexp.MustCompile(`(?i)^https?://(?:www\.)?github\.com/([^/\s]+)/([^/\s]+)$`)
)

// ParseGitHubRemoteURL extracts owner/repo from a git remote URL.
func ParseGitHubRemoteURL(remote string) (GitHubRepoRef, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return GitHubRepoRef{}, false
	}
	// strip trailing slash
	remote = strings.TrimRight(remote, "/")
	try := func(re *regexp.Regexp) (GitHubRepoRef, bool) {
		m := re.FindStringSubmatch(remote)
		if len(m) != 3 {
			return GitHubRepoRef{}, false
		}
		repo := strings.TrimSuffix(m[2], ".git")
		return GitHubRepoRef{Owner: m[1], Repo: repo}, true
	}
	if r, ok := try(githubSSHRE); ok {
		return r, true
	}
	if r, ok := try(githubHTTPSRE); ok {
		return r, true
	}
	return GitHubRepoRef{}, false
}

type catalogCacheEntry struct {
	repos []GitHubRepoRef
	at    time.Time
}

// ProjectRepoCatalog returns ordered GitHub repos for a project.
// Order: configured repos → legacy owner/repo → discover from git remotes (cached 5m).
func (c *Config) ProjectRepoCatalog(ctx context.Context, project string) ([]GitHubRepoRef, error) {
	return c.ProjectRepoCatalogWith(ctx, project, defaultGitRunner)
}

// ProjectRepoCatalogWith is ProjectRepoCatalog with an injectable git runner.
func (c *Config) ProjectRepoCatalogWith(ctx context.Context, project string, run GitRunner) ([]GitHubRepoRef, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("empty project")
	}
	if c == nil {
		return nil, fmt.Errorf("nil config")
	}
	c.mu.RLock()
	pc, ok := c.Projects[project]
	path := ""
	if ok {
		path = pc.Path
	}
	configured := pc.GitHub.NormalizedRepos()
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown project %q", project)
	}
	if len(configured) > 0 {
		return configured, nil
	}
	// Discover from git remotes.
	return c.discoverGitHubRepos(ctx, project, path, run)
}

func (c *Config) discoverGitHubRepos(ctx context.Context, project, path string, run GitRunner) ([]GitHubRepoRef, error) {
	if run == nil {
		run = defaultGitRunner
	}
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalogCache == nil {
		c.catalogCache = map[string]catalogCacheEntry{}
	}
	if e, ok := c.catalogCache[project]; ok && time.Since(e.at) < 5*time.Minute {
		return append([]GitHubRepoRef(nil), e.repos...), nil
	}
	var out []GitHubRepoRef
	seen := map[string]struct{}{}
	addURL := func(u string) {
		if r, ok := ParseGitHubRemoteURL(u); ok {
			key := strings.ToLower(r.Slug())
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			out = append(out, r)
		}
	}
	// origin first, then other remotes.
	if raw, err := run(ctx, path, "git", "remote", "get-url", "origin"); err == nil {
		addURL(string(raw))
	}
	if raw, err := run(ctx, path, "git", "remote"); err == nil {
		for _, name := range strings.Fields(string(raw)) {
			name = strings.TrimSpace(name)
			if name == "" || name == "origin" {
				continue
			}
			if u, err := run(ctx, path, "git", "remote", "get-url", name); err == nil {
				addURL(string(u))
			}
		}
	}
	c.catalogCache[project] = catalogCacheEntry{repos: out, at: time.Now()}
	return append([]GitHubRepoRef(nil), out...), nil
}

// ResolveRepoPicker picks owner/repo from query against catalog (default first).
func ResolveRepoPicker(catalog []GitHubRepoRef, owner, repo string) (GitHubRepoRef, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if len(catalog) == 0 {
		if owner != "" && repo != "" {
			return GitHubRepoRef{Owner: owner, Repo: repo}, nil
		}
		return GitHubRepoRef{}, fmt.Errorf("no GitHub repos configured or discovered for project")
	}
	if owner == "" && repo == "" {
		return catalog[0], nil
	}
	// Allow repo=owner/repo form in repo field only.
	if owner == "" && strings.Contains(repo, "/") {
		parts := strings.SplitN(repo, "/", 2)
		owner, repo = parts[0], parts[1]
	}
	for _, r := range catalog {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, repo) {
			return r, nil
		}
	}
	return GitHubRepoRef{}, fmt.Errorf("repo %s/%s not in project catalog", owner, repo)
}

// PreferDiscordChannel returns the channel ID for web-started threads for project.
func (c *Config) PreferDiscordChannel(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", fmt.Errorf("empty project")
	}
	if c == nil {
		return "", fmt.Errorf("nil config")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return "", fmt.Errorf("unknown project %q", project)
	}
	pref := strings.TrimSpace(pc.DiscordChannelID)
	if pref != "" {
		mapped, ok := c.Channels[pref]
		if !ok || mapped != project {
			return "", fmt.Errorf("projects.%s.discordChannelId %q must be in channels mapped to %q", project, pref, project)
		}
		return pref, nil
	}
	var hits []string
	for ch, proj := range c.Channels {
		if proj == project {
			hits = append(hits, ch)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no Discord channel mapped for project %q", project)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("multiple channels map to %q; set projects.%s.discordChannelId", project, project)
	}
}

// ValidatePreferredChannels checks every project's discordChannelId when set.
func (c *Config) ValidatePreferredChannels() error {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for name, pc := range c.Projects {
		pref := strings.TrimSpace(pc.DiscordChannelID)
		if pref == "" {
			continue
		}
		mapped, ok := c.Channels[pref]
		if !ok || mapped != name {
			return fmt.Errorf("projects.%s.discordChannelId %q must be in channels mapped to %q", name, pref, name)
		}
	}
	return nil
}

// SetProjectGitHubRepos sets the explicit repo catalog for a project (replaces legacy fields).
func (c *Config) SetProjectGitHubRepos(name string, repos []GitHubRepoRef) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	norm := make([]GitHubRepoRef, 0, len(repos))
	seen := map[string]struct{}{}
	for _, r := range repos {
		r.Owner = strings.TrimSpace(r.Owner)
		r.Repo = strings.TrimSpace(r.Repo)
		if !r.Valid() {
			continue
		}
		key := strings.ToLower(r.Slug())
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		norm = append(norm, r)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if len(norm) == 0 {
		pc.GitHub = nil
	} else {
		pc.GitHub = &ProjectGitHubConfig{Repos: norm}
	}
	c.Projects[name] = pc
	// catalogCache is only touched under catalogMu (lock order: c.mu then catalogMu).
	c.invalidateCatalogCacheLocked(name)
	return c.saveLocked()
}

// invalidateCatalogCacheLocked drops a project's discover cache entry.
// Caller must hold c.mu (or not need it); this takes catalogMu.
func (c *Config) invalidateCatalogCacheLocked(project string) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalogCache != nil {
		delete(c.catalogCache, project)
	}
}

// SetProjectDiscordChannel sets preferred Discord channel (must be mapped to project).
func (c *Config) SetProjectDiscordChannel(name, channelID string) error {
	name = strings.TrimSpace(name)
	channelID = strings.TrimSpace(channelID)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if channelID != "" {
		mapped, ok := c.Channels[channelID]
		if !ok || mapped != name {
			return fmt.Errorf("channel %q must be mapped to project %q in channels", channelID, name)
		}
	}
	pc.DiscordChannelID = channelID
	c.Projects[name] = pc
	return c.saveLocked()
}

// SetDiscordGuildID sets the global default guild id (fallback for projects
// without their own discordGuildId).
func (c *Config) SetDiscordGuildID(guildID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.DiscordGuildID = strings.TrimSpace(guildID)
	return c.saveLocked()
}

// DiscordGuildIDValue returns the global default guild id (not project-resolved).
func (c *Config) DiscordGuildIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.DiscordGuildID)
}

// ProjectDiscordGuildID returns the guild for Discord deep links for a project:
// projects.<name>.discordGuildId if set, else the global discordGuildId fallback.
func (c *Config) ProjectDiscordGuildID(project string) string {
	if c == nil {
		return ""
	}
	project = strings.TrimSpace(project)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if project != "" {
		if pc, ok := c.Projects[project]; ok {
			if g := strings.TrimSpace(pc.DiscordGuildID); g != "" {
				return g
			}
		}
	}
	return strings.TrimSpace(c.DiscordGuildID)
}

// SetProjectDiscordGuild sets the per-project Discord guild id (empty clears).
func (c *Config) SetProjectDiscordGuild(name, guildID string) error {
	name = strings.TrimSpace(name)
	guildID = strings.TrimSpace(guildID)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	pc.DiscordGuildID = guildID
	c.Projects[name] = pc
	return c.saveLocked()
}

// SetProjectDiscord updates preferred channel and/or guild for a project in one save.
// Empty channelID clears preferred channel; empty guildID clears project guild
// (global fallback still applies for deep links).
func (c *Config) SetProjectDiscord(name, channelID, guildID string) error {
	name = strings.TrimSpace(name)
	channelID = strings.TrimSpace(channelID)
	guildID = strings.TrimSpace(guildID)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if channelID != "" {
		mapped, ok := c.Channels[channelID]
		if !ok || mapped != name {
			return fmt.Errorf("channel %q must be mapped to project %q in channels", channelID, name)
		}
	}
	pc.DiscordChannelID = channelID
	pc.DiscordGuildID = guildID
	c.Projects[name] = pc
	return c.saveLocked()
}

// ChannelsForProject returns channel IDs mapped to the project (sorted by id).
func (c *Config) ChannelsForProject(project string) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for ch, proj := range c.Channels {
		if proj == project {
			out = append(out, ch)
		}
	}
	// sort without importing slices for tiny lists
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

