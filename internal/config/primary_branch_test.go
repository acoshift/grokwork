package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePrimaryBranchName(t *testing.T) {
	t.Parallel()
	ok := []string{"main", "prod", "develop", "release-1", "v2_0"}
	for _, in := range ok {
		if err := ValidatePrimaryBranchName(in); err != nil {
			t.Errorf("ValidatePrimaryBranchName(%q) = %v, want nil", in, err)
		}
	}
	bad := []string{
		"",
		"  ",
		"origin/main",
		"refs/heads/main",
		"release/2026.08",
		"-main",
		".hidden",
		"main.",
		"main.lock",
		"@",
		"a..b",
		"a@{b",
		"has space",
		"has~tilde",
		"has^caret",
		"has:colon",
		"has?q",
		"has*star",
		"has[brack",
		"has\\slash",
		"HEAD",
		strings.Repeat("a", MaxPrimaryBranchLen+1),
	}
	for _, in := range bad {
		if err := ValidatePrimaryBranchName(in); err == nil {
			t.Errorf("ValidatePrimaryBranchName(%q) should have failed", in)
		}
	}
}

func TestProjectPrimaryBranchSetClearRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}

	if got := cfg.ProjectPrimaryBranch("app"); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	if err := cfg.SetProjectPrimaryBranch("app", "prod"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectPrimaryBranch("app"); got != "prod" {
		t.Fatalf("got %q want prod", got)
	}
	if snap := cfg.Snapshot(); len(snap.Projects) == 0 || snap.Projects[0].PrimaryBranch != "prod" {
		t.Fatalf("snapshot: %+v", snap.Projects)
	}

	m := ProjectsMap{"app": cfg.Projects["app"]}
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"primaryBranch":"prod"`) {
		t.Fatalf("marshal dropped primaryBranch: %s", b)
	}
	var back ProjectsMap
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if got := back["app"].PrimaryBranch; got != "prod" {
		t.Fatalf("unmarshal: got %q", got)
	}
	if got := cloneProjectsMap(m)["app"].PrimaryBranch; got != "prod" {
		t.Fatalf("clone: got %q", got)
	}

	if err := cfg.SetProjectPrimaryBranch("app", "origin/main"); err == nil {
		t.Fatal("expected reject origin/main")
	}
	if got := cfg.ProjectPrimaryBranch("app"); got != "prod" {
		t.Fatalf("reject should not change: got %q", got)
	}
	if err := cfg.SetProjectPrimaryBranch("app", ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectPrimaryBranch("app"); got != "" {
		t.Fatalf("clear: got %q", got)
	}
	if err := cfg.SetProjectPrimaryBranch("missing", "main"); err == nil {
		t.Fatal("expected unknown project error")
	}
}

func TestProjectPrimaryBranchInvalidEffectiveEmpty(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/tmp", PrimaryBranch: "origin/main"},
		},
	}
	if got := cfg.ProjectPrimaryBranch("app"); got != "" {
		t.Fatalf("invalid effective: got %q want empty", got)
	}
	if got := cfg.Projects["app"].PrimaryBranch; got != "origin/main" {
		t.Fatalf("raw: got %q", got)
	}
	snap := cfg.Snapshot()
	if len(snap.Projects) != 1 {
		t.Fatalf("snapshot len %d", len(snap.Projects))
	}
	if snap.Projects[0].PrimaryBranch != "origin/main" {
		t.Fatalf("snapshot raw: %q", snap.Projects[0].PrimaryBranch)
	}
	if !snap.Projects[0].PrimaryBranchInvalid {
		t.Fatal("snapshot should mark invalid")
	}
}
