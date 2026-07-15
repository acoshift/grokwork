package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	DiscordToken   string            `json:"discordToken"`
	AllowedUserIDs []string          `json:"allowedUserIds"`
	AllowedRoleIDs []string          `json:"allowedRoleIds"`
	Projects       map[string]string `json:"projects"`
	Channels       map[string]string `json:"channels"` // channel ID → project name
	GrokBin        string            `json:"grokBin"`
	Yolo           *bool             `json:"yolo"`
	Model          string            `json:"model"`
	MaxTurns       int               `json:"maxTurns"`
	TimeoutMs      int               `json:"timeoutMs"`
	ExtraArgs            []string `json:"extraArgs"`
	SummarizeThreadTitle *bool    `json:"summarizeThreadTitle"`
	SummarizeTimeoutMs   int      `json:"summarizeTimeoutMs"`
	WorktreeIsolation    *bool    `json:"worktreeIsolation"`

	AllowedUsers map[string]struct{} `json:"-"`
	AllowedRoles map[string]struct{} `json:"-"`
	DataDir      string              `json:"-"`
	ConfigPath   string              `json:"-"`
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

func toSet(ids []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			m[id] = struct{}{}
		}
	}
	return m
}
