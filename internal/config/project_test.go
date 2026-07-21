package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectsMapDualShape(t *testing.T) {
	raw := []byte(`{
		"stringy": "/tmp/a",
		"objecty": {
			"path": "/tmp/b",
			"linear": { "enabled": true, "apiKey": "k1", "teamKey": "ENG" }
		}
	}`)
	var m ProjectsMap
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["stringy"].Path != "/tmp/a" || m["stringy"].Linear != nil {
		t.Fatalf("stringy: %+v", m["stringy"])
	}
	if m["objecty"].Path != "/tmp/b" || !m["objecty"].Linear.Enabled || m["objecty"].Linear.APIKey != "k1" {
		t.Fatalf("objecty: %+v", m["objecty"])
	}

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var round ProjectsMap
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatal(err)
	}
	if !round["objecty"].Linear.Enabled || round["stringy"].Path != "/tmp/a" {
		t.Fatalf("round: %+v", round)
	}
}

func TestProjectLinearAccessorsAndEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"homeconnect": {
				Path: filepath.Join(dir, "hc"),
				Linear: &ProjectLinearConfig{
					Enabled: true,
					TeamKey: "ENG",
				},
			},
			"plain": {Path: filepath.Join(dir, "p")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "hc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !cfg.ProjectLinearEnabled("homeconnect") {
		t.Fatal("expected enabled")
	}
	if cfg.ProjectLinearEnabled("plain") {
		t.Fatal("plain should be off")
	}
	if cfg.ProjectLinearAPIKey("homeconnect") != "" {
		t.Fatal("no key yet")
	}
	t.Setenv("LINEAR_API_KEY_HOMECONNECT", "from-env")
	if got := cfg.ProjectLinearAPIKey("homeconnect"); got != "from-env" {
		t.Fatalf("env key=%q", got)
	}
	if !cfg.ProjectLinearCanResolve("homeconnect") {
		t.Fatal("can resolve with env")
	}

	if err := cfg.SetProjectLinear("homeconnect", true, "ENG", "from-config", false); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectLinearAPIKey("homeconnect"); got != "from-config" {
		t.Fatalf("config wins over env: %q", got)
	}

	// Empty apiKey on set leaves key unchanged.
	if err := cfg.SetProjectLinear("homeconnect", true, "HAH", "", false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectLinearTeamKey("homeconnect") != "HAH" {
		t.Fatal(cfg.ProjectLinearTeamKey("homeconnect"))
	}
	if cfg.ProjectLinearAPIKey("homeconnect") != "from-config" {
		t.Fatal("key cleared unexpectedly")
	}

	if ProjectEnvKeySuffix("hah-platform") != "HAH_PLATFORM" {
		t.Fatalf("%q", ProjectEnvKeySuffix("hah-platform"))
	}
}

func TestProjectRepoFetchInterval(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"app":  {Path: filepath.Join(dir, "app")},
			"fast": {Path: filepath.Join(dir, "fast")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fast"), 0o755); err != nil {
		t.Fatal(err)
	}

	if cfg.ProjectRepoFetchIntervalMinutes("app") != DefaultRepoFetchIntervalMinutes {
		t.Fatalf("default minutes=%d", cfg.ProjectRepoFetchIntervalMinutes("app"))
	}
	if cfg.ProjectRepoFetchInterval("app") != time.Duration(DefaultRepoFetchIntervalMinutes)*time.Minute {
		t.Fatalf("default dur=%v", cfg.ProjectRepoFetchInterval("app"))
	}
	// Unknown project still gets default (not zero).
	if cfg.ProjectRepoFetchIntervalMinutes("missing") != DefaultRepoFetchIntervalMinutes {
		t.Fatal("missing project should use default")
	}

	if err := cfg.SetProjectRepoFetchIntervalMinutes("app", 0); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRepoFetchIntervalMinutes("app") != 0 || cfg.ProjectRepoFetchInterval("app") != 0 {
		t.Fatalf("0 should disable: mins=%d dur=%v",
			cfg.ProjectRepoFetchIntervalMinutes("app"), cfg.ProjectRepoFetchInterval("app"))
	}
	if err := cfg.SetProjectRepoFetchIntervalMinutes("fast", 15); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRepoFetchIntervalMinutes("fast") != 15 {
		t.Fatalf("fast=%d", cfg.ProjectRepoFetchIntervalMinutes("fast"))
	}
	if err := cfg.SetProjectRepoFetchIntervalMinutes("app", -1); err == nil {
		t.Fatal("expected reject negative")
	}

	// Round-trip via ProjectsMap JSON.
	raw, err := json.Marshal(cfg.Projects)
	if err != nil {
		t.Fatal(err)
	}
	var m ProjectsMap
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["app"].RepoFetchIntervalMinutes == nil || *m["app"].RepoFetchIntervalMinutes != 0 {
		t.Fatalf("app interval after marshal: %v", m["app"].RepoFetchIntervalMinutes)
	}
	if m["fast"].RepoFetchIntervalMinutes == nil || *m["fast"].RepoFetchIntervalMinutes != 15 {
		t.Fatalf("fast interval after marshal: %v", m["fast"].RepoFetchIntervalMinutes)
	}

	// Snapshot shows effective values.
	snap := cfg.Snapshot()
	var appItem, fastItem ProjectItem
	for _, p := range snap.Projects {
		switch p.Name {
		case "app":
			appItem = p
		case "fast":
			fastItem = p
		}
	}
	if appItem.RepoFetchIntervalMinutes != 0 {
		t.Fatalf("snapshot app=%d", appItem.RepoFetchIntervalMinutes)
	}
	if fastItem.RepoFetchIntervalMinutes != 15 {
		t.Fatalf("snapshot fast=%d", fastItem.RepoFetchIntervalMinutes)
	}

	targets := cfg.IdleRepoFetchTargets()
	// app disabled (0); fast enabled at 15m.
	var sawApp, sawFast bool
	for _, tgt := range targets {
		switch tgt.Name {
		case "app":
			sawApp = true
			if tgt.Interval != 0 {
				t.Fatalf("app interval=%v", tgt.Interval)
			}
		case "fast":
			sawFast = true
			if tgt.Interval != 15*time.Minute {
				t.Fatalf("fast interval=%v", tgt.Interval)
			}
		}
	}
	if !sawApp || !sawFast {
		t.Fatalf("targets=%+v", targets)
	}
}

func TestLoadStringProjectsStillWorks(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := []byte(`{
		"discordToken": "tok",
		"projects": { "p": "` + proj + `" },
		"channels": { "c1": "p" }
	}`)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	path, ok := cfg.ProjectPath("p")
	if !ok || path != proj {
		t.Fatalf("%q %v", path, ok)
	}
}
