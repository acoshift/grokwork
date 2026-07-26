package config

import (
	"testing"
)

func TestAccessAllowedProjectOnly(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"a": {Path: "/a", AllowedUserIDs: []string{"u-a"}},
			"b": {Path: "/b", AllowedUserIDs: []string{"u-b"}},
			"c": {Path: "/c"},
		},
	}
	if !cfg.AccessAllowed("a", "u-a") {
		t.Fatal("u-a on a")
	}
	if cfg.AccessAllowed("b", "u-a") {
		t.Fatal("u-a must not access b")
	}
	if cfg.AccessAllowed("a", "other") {
		t.Fatal("a non-member must not access a")
	}
	if cfg.AccessAllowed("c", "u-a") {
		t.Fatal("empty project fail-closed")
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
