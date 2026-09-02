package config

import (
	"encoding/json"
	"os"
	"testing"
)

// TestExampleConfigParses pins that config.example.json — the file operators
// copy — still round-trips through the real types. A doc example that cannot
// load is worse than no example.
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json does not parse: %v", err)
	}
	pc, ok := cfg.Projects["app"]
	if !ok {
		t.Fatal("example lost the app project")
	}
	if pc.Deploy == nil {
		t.Fatal("example deploy block was dropped by normalization")
	}
	prod := pc.Deploy.Environments["prod"]
	if prod == nil || prod.RequireCapability != "approve" {
		t.Fatalf("example prod policy = %+v", prod)
	}
	if len(prod.AllowedRefs) != 1 || prod.AllowedRefs[0] != "main" {
		t.Fatalf("example prod allowedRefs = %v", prod.AllowedRefs)
	}
	// The example marks DB_URL secret with an empty value; normalization keeps
	// the marking (the key exists) but it contributes no redaction target.
	if !prod.IsSecretKey("DB_URL") {
		t.Fatalf("example lost the DB_URL secret marking: %v", prod.SecretKeys)
	}
	if vals := prod.SecretValues(); len(vals) != 0 {
		t.Fatalf("an empty secret value became a redaction target %v; it would match everywhere", vals)
	}
	if cfg.MaxConcurrentDeploys == nil || *cfg.MaxConcurrentDeploys != 4 {
		t.Fatalf("example maxConcurrentDeploys = %v", cfg.MaxConcurrentDeploys)
	}
	if cfg.Model != "grok-4.6-high" {
		t.Fatalf("example model = %q, want grok-4.6-high", cfg.Model)
	}
}
