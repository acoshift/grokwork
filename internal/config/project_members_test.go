package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAccessAllowedProjectOnly(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"a": {Path: "/a", AllowedUserIDs: []string{"u-a"}, AllowedRoleIDs: []string{"r-a"}},
			"b": {Path: "/b", AllowedUserIDs: []string{"u-b"}},
			"c": {Path: "/c"},
		},
	}
	if !cfg.AccessAllowed("a", "u-a", nil) {
		t.Fatal("u-a on a")
	}
	if cfg.AccessAllowed("b", "u-a", nil) {
		t.Fatal("u-a must not access b")
	}
	if !cfg.AccessAllowed("a", "other", []string{"r-a"}) {
		t.Fatal("role r-a on a")
	}
	if cfg.AccessAllowed("c", "u-a", nil) {
		t.Fatal("empty project fail-closed")
	}
}

func TestMigrateGlobalAllowlistToProjects(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &Config{
		AllowedUserIDs: []string{"g1", "g2"},
		AllowedRoleIDs: []string{"gr"},
		Projects: ProjectsMap{
			"empty": {Path: "/e"},
			"seeded": {Path: "/s", AllowedUserIDs: []string{"keep"}},
		},
		ConfigPath: cfgPath,
	}
	n, err := cfg.MigrateGlobalAllowlistToProjects()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated n=%d", n)
	}
	if !cfg.AccessAllowed("empty", "g1", nil) {
		t.Fatal("empty should have global users")
	}
	if cfg.AccessAllowed("seeded", "g1", nil) {
		t.Fatal("seeded must not be overwritten")
	}
	if !cfg.AccessAllowed("seeded", "keep", nil) {
		t.Fatal("seeded keep")
	}
	if len(cfg.AllowedUserIDs) != 0 || len(cfg.AllowedRoleIDs) != 0 {
		t.Fatalf("global not cleared: %v %v", cfg.AllowedUserIDs, cfg.AllowedRoleIDs)
	}
	n2, err := cfg.MigrateGlobalAllowlistToProjects()
	if err != nil || n2 != 0 {
		t.Fatalf("second migrate n=%d err=%v", n2, err)
	}
	raw, _ := os.ReadFile(cfgPath)
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if _, ok := disk["allowedUserIds"]; ok {
		// omitempty may omit empty — ok either way
	}
}

func TestProjectsVisibleTo(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"a": {Path: "/a", AllowedUserIDs: []string{"u1"}},
			"b": {Path: "/b", AllowedUserIDs: []string{"u2"}},
		},
	}
	got := cfg.ProjectsVisibleTo("u1", WebRoleMember)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("visible=%v", got)
	}
	all := cfg.ProjectsVisibleTo("u1", WebRoleAdmin)
	if len(all) != 2 {
		t.Fatalf("admin visible=%v", all)
	}
}
