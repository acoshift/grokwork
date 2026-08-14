package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveLockedPreservesAgentMCP guards the kill-switch: agentMCP must survive
// saveLocked (any web config save). Dropping the field reverts to default-on.
func TestSaveLockedPreservesAgentMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	off := false
	cfg := &Config{
		DiscordToken: "test-token",
		ConfigPath:   path,
		Projects:     PathProjects(map[string]string{"app": dir}),
		Channels:     map[string]string{},
		AgentMCP:     &off,
		DataDir:      dir,
	}
	cfg.mu.Lock()
	err := cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if again.AgentMCP == nil || *again.AgentMCP != false {
		t.Fatalf("agentMCP lost on save: raw=%s", raw)
	}
	if again.AgentMCPEnabled() {
		t.Fatal("AgentMCPEnabled must be false")
	}
}
