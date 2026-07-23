package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// saveLocked must persist Wave-1 root fields or web config Save wipes them.
func TestSaveLockedPreservesWave1RootFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Minimal loadable config
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+dir+`", "allowedUserIds": ["u1"]}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Load via reading JSON into Config then saveLocked path
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	cfg.DataDir = dir
	max := 4
	maxU := 2
	cfg.MaxConcurrentRuns = &max
	cfg.MaxConcurrentRunsUser = &maxU
	cfg.GrokEnvDenylist = []string{"CUSTOM_SECRET_"}
	cfg.DiscordUserGitHub = map[string]GitHubIdentity{
		"u9": {Login: "nine", Name: "Nine"},
	}

	cfg.mu.Lock()
	err = cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if again.MaxConcurrentRuns == nil || *again.MaxConcurrentRuns != 4 {
		t.Fatalf("MaxConcurrentRuns lost: %+v", again.MaxConcurrentRuns)
	}
	if again.MaxConcurrentRunsUser == nil || *again.MaxConcurrentRunsUser != 2 {
		t.Fatalf("MaxConcurrentRunsUser lost: %+v", again.MaxConcurrentRunsUser)
	}
	if len(again.GrokEnvDenylist) != 1 || again.GrokEnvDenylist[0] != "CUSTOM_SECRET_" {
		t.Fatalf("GrokEnvDenylist lost: %v", again.GrokEnvDenylist)
	}
	if again.DiscordUserGitHub["u9"].Login != "nine" {
		t.Fatalf("DiscordUserGitHub lost: %+v", again.DiscordUserGitHub)
	}
}
