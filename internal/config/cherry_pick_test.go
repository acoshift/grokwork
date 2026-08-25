package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCherryPickTargetsSetClearRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if got := cfg.ProjectCherryPickTargets("app"); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	if err := cfg.SetProjectCherryPickTargets("app", []string{"staging", "production"}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectCherryPickTargets("app"); !slices.Equal(got, []string{"staging", "production"}) {
		t.Fatalf("got %v", got)
	}
	snap := cfg.Snapshot()
	if len(snap.Projects) == 0 || snap.Projects[0].CherryPickTargetsText != "staging\nproduction" {
		t.Fatalf("snapshot text: %+v", snap.Projects)
	}
	if !slices.Equal(snap.Projects[0].CherryPickTargets, []string{"staging", "production"}) {
		t.Fatalf("snapshot effective: %v", snap.Projects[0].CherryPickTargets)
	}

	m := ProjectsMap{"app": cfg.Projects["app"]}
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cherryPickTargets"`) {
		t.Fatalf("marshal dropped cherryPickTargets: %s", b)
	}
	var back ProjectsMap
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if got := cloneProjectsMap(m)["app"].CherryPickTargets; !slices.Equal(got, []string{"staging", "production"}) {
		t.Fatalf("clone: %v", got)
	}
	cloned := cloneProjectsMap(m)
	cloned["app"].CherryPickTargets[0] = "mutated"
	if cfg.Projects["app"].CherryPickTargets[0] != "staging" {
		t.Fatal("clone must detach the slice")
	}

	if err := cfg.SetProjectCherryPickTargets("app", []string{"origin/staging"}); err == nil {
		t.Fatal("expected reject origin/staging")
	}
	if err := cfg.SetProjectPrimaryBranch("app", "prod"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectCherryPickTargets("app"); !slices.Equal(got, []string{"staging", "production"}) {
		t.Fatalf("unrelated mutate dropped targets: %v", got)
	}

	if err := cfg.SetProjectCherryPickTargets("app", nil); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectCherryPickTargets("app"); len(got) != 0 {
		t.Fatalf("clear: %v", got)
	}
	if err := cfg.SetProjectCherryPickTargets("missing", []string{"staging"}); err == nil {
		t.Fatal("expected unknown project")
	}
}

func TestCherryPickTargetsInvalidOmittedFromEffective(t *testing.T) {
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {Path: "/tmp", CherryPickTargets: []string{"staging", "origin/prod", "production"}},
		},
	}
	if got := cfg.ProjectCherryPickTargets("app"); !slices.Equal(got, []string{"staging", "production"}) {
		t.Fatalf("effective: %v", got)
	}
	snap := cfg.Snapshot()
	if !snap.Projects[0].CherryPickTargetsInvalid {
		t.Fatal("snapshot should mark invalid")
	}
	if snap.Projects[0].CherryPickTargetsText != "staging\norigin/prod\nproduction" {
		t.Fatalf("raw text: %q", snap.Projects[0].CherryPickTargetsText)
	}
}

func TestForcePushTargetsSetClearRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if cfg.ProjectForcePushTargets("app") {
		t.Fatal("empty should be cherry-pick")
	}
	if err := cfg.SetProjectCherryPickConfig("app", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectForcePushTargets("app") {
		t.Fatal("want force-push")
	}
	if got := cfg.ProjectCherryPickTargets("app"); !slices.Equal(got, []string{"staging"}) {
		t.Fatalf("targets: %v", got)
	}
	snap := cfg.Snapshot()
	if len(snap.Projects) == 0 || !snap.Projects[0].ForcePushTargets {
		t.Fatalf("snapshot force: %+v", snap.Projects)
	}

	m := ProjectsMap{"app": cfg.Projects["app"]}
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"forcePushTargets"`) {
		t.Fatalf("marshal dropped forcePushTargets: %s", b)
	}
	var back ProjectsMap
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if back["app"].ForcePushTargets == nil || !*back["app"].ForcePushTargets {
		t.Fatal("unmarshal dropped forcePushTargets")
	}
	cloned := cloneProjectsMap(m)
	if cloned["app"].ForcePushTargets == nil || !*cloned["app"].ForcePushTargets {
		t.Fatal("clone dropped forcePushTargets")
	}
	*cloned["app"].ForcePushTargets = false
	if cfg.Projects["app"].ForcePushTargets == nil || !*cfg.Projects["app"].ForcePushTargets {
		t.Fatal("clone must detach the bool")
	}

	if err := cfg.SetProjectCherryPickTargets("app", []string{"staging", "production"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectForcePushTargets("app") {
		t.Fatal("SetProjectCherryPickTargets must leave force flag intact")
	}
	if got := cfg.ProjectCherryPickTargets("app"); !slices.Equal(got, []string{"staging", "production"}) {
		t.Fatalf("targets after leave-intact: %v", got)
	}

	if err := cfg.SetProjectCherryPickConfig("app", []string{"staging"}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectForcePushTargets("app") {
		t.Fatal("cleared force")
	}
	b, err = ProjectsMap{"app": cfg.Projects["app"]}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"forcePushTargets"`) {
		t.Fatalf("false should omit forcePushTargets: %s", b)
	}

	if err := cfg.SetProjectCherryPickConfig("missing", []string{"staging"}, true); err == nil {
		t.Fatal("expected unknown project")
	}
}

func TestCherryPickTargetsCap(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	tooMany := make([]string, MaxCherryPickTargets+1)
	for i := range tooMany {
		tooMany[i] = "b" + strings.Repeat("x", i)
	}
	if err := cfg.SetProjectCherryPickTargets("app", tooMany); err == nil {
		t.Fatal("expected cap error")
	}
}
