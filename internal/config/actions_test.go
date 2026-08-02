package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func actionsTestConfig(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "discordToken": "tok",
  "projects": {"app": {"path": "` + dir + `", "allowedUserIds": ["u1"]}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
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
	return &cfg, path
}

func TestActionsDispatchBranchesUnlocked(t *testing.T) {
	cfg, _ := actionsTestConfig(t)
	branches, locked := cfg.ActionsDispatchBranches("app", "acme", "app", ".github/workflows/ci.yml")
	if locked || branches != nil {
		t.Fatalf("unlocked want nil,false got %v,%v", branches, locked)
	}
	// Unknown project.
	if _, locked := cfg.ActionsDispatchBranches("nope", "a", "b", "ci.yml"); locked {
		t.Fatal("unknown project should be unlocked")
	}
}

func TestActionsDispatchBranchesBaseNameAndCase(t *testing.T) {
	cfg, _ := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Workflow: "Deploy.YML", Branches: []string{"production", "staging"}},
		},
	}
	cfg.Projects["app"] = pc

	branches, locked := cfg.ActionsDispatchBranches("app", "acme", "app", ".github/workflows/deploy.yml")
	if !locked {
		t.Fatal("expected locked")
	}
	if !slices.Equal(branches, []string{"production", "staging"}) {
		t.Fatalf("branches=%v", branches)
	}
}

func TestActionsDispatchBranchesRepoSpecificBeatsWildcard(t *testing.T) {
	cfg, _ := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Workflow: "deploy.yml", Branches: []string{"main"}},
			{Repo: "shop", Workflow: "deploy.yml", Branches: []string{"production"}},
			{Repo: "acme/other", Workflow: "deploy.yml", Branches: []string{"canary"}},
		},
	}
	cfg.Projects["app"] = pc

	// Repo name match.
	b, locked := cfg.ActionsDispatchBranches("app", "acme", "shop", ".github/workflows/deploy.yml")
	if !locked || !slices.Equal(b, []string{"production"}) {
		t.Fatalf("shop: %v locked=%v", b, locked)
	}
	// owner/repo match.
	b, locked = cfg.ActionsDispatchBranches("app", "acme", "other", "deploy.yml")
	if !locked || !slices.Equal(b, []string{"canary"}) {
		t.Fatalf("other: %v locked=%v", b, locked)
	}
	// Falls back to wildcard.
	b, locked = cfg.ActionsDispatchBranches("app", "acme", "unlisted", "deploy.yml")
	if !locked || !slices.Equal(b, []string{"main"}) {
		t.Fatalf("wildcard: %v locked=%v", b, locked)
	}
	// Different workflow → unlocked.
	if _, locked := cfg.ActionsDispatchBranches("app", "acme", "shop", "ci.yml"); locked {
		t.Fatal("other workflow should be unlocked")
	}
}

func TestActionsDispatchBranchesRepoCaseInsensitive(t *testing.T) {
	cfg, _ := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Repo: "Acme/Shop", Workflow: "deploy.yml", Branches: []string{"prod"}},
		},
	}
	cfg.Projects["app"] = pc
	b, locked := cfg.ActionsDispatchBranches("app", "acme", "shop", "deploy.yml")
	if !locked || !slices.Equal(b, []string{"prod"}) {
		t.Fatalf("%v locked=%v", b, locked)
	}
}

func TestActionsDispatchBranchesReturnsCopy(t *testing.T) {
	cfg, _ := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Workflow: "deploy.yml", Branches: []string{"production"}},
		},
	}
	cfg.Projects["app"] = pc

	b, locked := cfg.ActionsDispatchBranches("app", "", "app", "deploy.yml")
	if !locked || len(b) != 1 {
		t.Fatalf("%v locked=%v", b, locked)
	}
	b[0] = "MUTATED"
	again, _ := cfg.ActionsDispatchBranches("app", "", "app", "deploy.yml")
	if again[0] != "production" {
		t.Fatalf("mutated backing array: %v", again)
	}
	// And the live config slice is intact.
	if cfg.Projects["app"].Actions.DispatchRules[0].Branches[0] != "production" {
		t.Fatal("config mutated")
	}
}

func TestActionsJSONRoundTripAndClone(t *testing.T) {
	cfg, path := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Repo: "shop", Workflow: "deploy.yml", Branches: []string{"production", "production", " staging ", ""}},
			{Workflow: "", Branches: []string{"main"}},                // dropped: empty workflow
			{Workflow: "ci.yml", Branches: nil},                       // dropped: empty branches
			{Workflow: "  release.yml  ", Branches: []string{"main"}}, // trimmed
		},
	}
	cfg.Projects["app"] = pc

	// saveLocked goes through MarshalJSON whitelist.
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
		t.Fatalf("reload: %v\n%s", err, raw)
	}
	act := again.Projects["app"].Actions
	if act == nil {
		t.Fatal("actions lost on round-trip")
	}
	if len(act.DispatchRules) != 2 {
		t.Fatalf("rules=%+v (expected 2 after normalize)", act.DispatchRules)
	}
	// Dedupe + trim branches; empty workflow/branch rules dropped.
	r0 := act.DispatchRules[0]
	if r0.Repo != "shop" || r0.Workflow != "deploy.yml" {
		t.Fatalf("r0=%+v", r0)
	}
	if !slices.Equal(r0.Branches, []string{"production", "staging"}) {
		t.Fatalf("r0 branches=%v", r0.Branches)
	}
	r1 := act.DispatchRules[1]
	if r1.Workflow != "release.yml" || !slices.Equal(r1.Branches, []string{"main"}) {
		t.Fatalf("r1=%+v", r1)
	}

	// cloneProjectsMap detaches branch slices.
	cloned := cloneProjectsMap(again.Projects)
	cloned["app"].Actions.DispatchRules[0].Branches[0] = "MUTATED"
	if again.Projects["app"].Actions.DispatchRules[0].Branches[0] != "production" {
		t.Fatal("cloneProjectsMap shares Branches slice")
	}
}

func TestActionsNormalizeEmptyToNil(t *testing.T) {
	// All rules invalid → Actions becomes nil, not a pointer to empty.
	raw := []byte(`{
  "discordToken": "tok",
  "projects": {
    "app": {
      "path": "/tmp",
      "actions": {
        "dispatchRules": [
          {"workflow": "", "branches": ["main"]},
          {"workflow": "ci.yml", "branches": []}
        ]
      }
    }
  },
  "channels": {},
  "grokBin": "grok"
}`)
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Projects["app"].Actions != nil {
		t.Fatalf("want nil Actions, got %+v", cfg.Projects["app"].Actions)
	}
}

func TestProjectWhitelistPreservesActions(t *testing.T) {
	cfg, path := actionsTestConfig(t)
	pc := cfg.Projects["app"]
	pc.Actions = &ProjectActionsConfig{
		DispatchRules: []ActionsDispatchRule{
			{Workflow: "deploy.yml", Branches: []string{"production"}},
		},
	}
	cfg.Projects["app"] = pc

	// Unrelated save must not drop actions (MarshalJSON outObj trap).
	if err := cfg.SetProjectVerifyCommands("app", []VerifyCommand{{Name: "t", Command: "go test"}}); err != nil {
		t.Fatal(err)
	}
	again := reloadConfig(t, path)
	act := again.Projects["app"].Actions
	if act == nil || len(act.DispatchRules) != 1 || act.DispatchRules[0].Workflow != "deploy.yml" {
		t.Fatalf("actions lost: %+v", again.Projects["app"])
	}
}
