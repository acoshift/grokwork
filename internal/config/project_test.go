package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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

func TestLoadStringProjectsStillWorks(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := []byte(`{
		"discordToken": "tok",
		"allowedUserIds": ["u1"],
		"allowedRoleIds": [],
		"projects": { "p": "` + proj + `" },
		"channels": { "c1": "p" }
	}`)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_DISCORD_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	t.Setenv("GROK_DISCORD_HTTP_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	path, ok := cfg.ProjectPath("p")
	if !ok || path != proj {
		t.Fatalf("%q %v", path, ok)
	}
}
