package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVerifyCommandsText(t *testing.T) {
	cmds, err := ParseVerifyCommandsText(`
# comment
unit | go test ./...
lint | make lint | 300000
fmt: gofmt -l .
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 3 {
		t.Fatalf("len=%d %+v", len(cmds), cmds)
	}
	if cmds[0].Name != "unit" || cmds[0].Command != "go test ./..." || cmds[0].TimeoutMs != 0 {
		t.Fatalf("%+v", cmds[0])
	}
	if cmds[1].TimeoutMs != 300000 {
		t.Fatalf("%+v", cmds[1])
	}
	if cmds[2].Name != "fmt" || cmds[2].Command != "gofmt -l ." {
		t.Fatalf("%+v", cmds[2])
	}
	text := FormatVerifyCommandsText(cmds[:2])
	if text != "unit | go test ./...\nlint | make lint | 300000" {
		t.Fatalf("format=%q", text)
	}
	if _, err := ParseVerifyCommandsText("bad line no separator"); err == nil {
		t.Fatal("want parse error")
	}
	dir := t.TempDir()
	cfg := &Config{
		Projects:   PathProjects(map[string]string{"app": dir}),
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := cfg.SetProjectVerifyCommands("app", cmds[:2]); err != nil {
		t.Fatal(err)
	}
	snap := cfg.Snapshot()
	var item *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "app" {
			item = &snap.Projects[i]
			break
		}
	}
	if item == nil || !strings.Contains(item.VerifyCommandsText, "unit |") {
		t.Fatalf("snapshot text: %+v", item)
	}
}

func TestProjectSafeTeam(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"support": {
				Path:           filepath.Join(dir, "support"),
				AllowedUserIDs: []string{"u1", "u2"},
			},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if cfg.SafeTeamMode("support") {
		t.Fatal("default SafeTeamMode off")
	}
	if err := cfg.SetProjectSafeTeam("support", true, "investigator", "case"); err != nil {
		t.Fatal(err)
	}
	if !cfg.SafeTeamMode("support") {
		t.Fatal("want SafeTeamMode on")
	}
	if cfg.ProjectDefaultMode("support") != "case" {
		t.Fatalf("defaultMode=%q", cfg.ProjectDefaultMode("support"))
	}
	if cfg.SafeTeamDefaultTemplate("support") != "investigator" {
		t.Fatalf("default tpl=%q", cfg.SafeTeamDefaultTemplate("support"))
	}
	// Unmapped under safe → investigator, cannot ship.
	if cfg.ResolveCapabilities("support", "u1").CanShip() {
		t.Fatal("unmapped must not ship under SafeTeamMode")
	}
	if err := cfg.SetProjectTeam("support", "eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProjectTeamMember("support", "eng", "eng-1"); err != nil {
		t.Fatal(err)
	}
	if !cfg.ResolveCapabilities("support", "eng-1").CanShip() {
		t.Fatal("builder team member should ship")
	}
	if err := cfg.SetProjectCapabilityByUser("support", "u2", "approver"); err != nil {
		t.Fatal(err)
	}
	if !cfg.ResolveCapabilities("support", "u2").Approve {
		t.Fatal("user map approver")
	}
	snap := cfg.Snapshot()
	var item *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "support" {
			item = &snap.Projects[i]
			break
		}
	}
	if item == nil || !item.SafeTeamMode || item.DefaultMode != "case" {
		t.Fatalf("snapshot: %+v", item)
	}
	if len(item.UnmappedUserIDs) != 1 || item.UnmappedUserIDs[0] != "u1" {
		t.Fatalf("unmapped users: %v", item.UnmappedUserIDs)
	}
	if len(item.Teams) != 1 || item.Teams[0].Key != "eng" || item.Teams[0].Capabilities != "builder" {
		t.Fatalf("teams: %+v", item.Teams)
	}
	if item.Teams[0].Label != "Engineering" || item.Teams[0].TemplateUnknown {
		t.Fatalf("team row: %+v", item.Teams[0])
	}
	if len(item.MemberIDs) != 3 {
		t.Fatalf("member union: %v", item.MemberIDs)
	}
	if err := cfg.SetProjectSafeTeam("support", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.SafeTeamMode("support") {
		t.Fatal("want off after clear")
	}
	// Invalid mode / template.
	if err := cfg.SetProjectSafeTeam("support", true, "investigator", "nope"); err == nil {
		t.Fatal("want error for bad defaultMode")
	}
	if err := cfg.SetProjectSafeTeam("support", true, "not-a-tpl", "case"); err == nil {
		t.Fatal("want error for bad template")
	}
	if err := cfg.RemoveProjectCapabilityByUser("support", "u2"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RemoveProjectTeam("support", "eng"); err != nil {
		t.Fatal(err)
	}
	// The grant goes with the team: eng-1 loses both access and capabilities.
	if len(cfg.ProjectTeams("support")) != 0 {
		t.Fatalf("team survived removal: %+v", cfg.ProjectTeams("support"))
	}
	if cfg.AccessAllowed("support", "eng-1") {
		t.Fatal("removing the team must remove the access it granted")
	}
}

func TestProjectSafeTeamPolicyAndDefaultMode(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"support": {Path: filepath.Join(dir, "support")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := cfg.SetProjectDefaultMode("support", "case"); err != nil {
		t.Fatal(err)
	}
	// Policy save must not clobber the separately-saved default mode.
	if err := cfg.SetProjectSafeTeamPolicy("support", true, "operator"); err != nil {
		t.Fatal(err)
	}
	if !cfg.SafeTeamMode("support") {
		t.Fatal("want SafeTeamMode on")
	}
	if cfg.SafeTeamDefaultTemplate("support") != "operator" {
		t.Fatalf("default tpl=%q", cfg.SafeTeamDefaultTemplate("support"))
	}
	if cfg.ProjectDefaultMode("support") != "case" {
		t.Fatalf("defaultMode=%q (policy save clobbered it)", cfg.ProjectDefaultMode("support"))
	}
	// And mode saves must not clobber policy.
	if err := cfg.SetProjectDefaultMode("support", "investigate"); err != nil {
		t.Fatal(err)
	}
	if !cfg.SafeTeamMode("support") || cfg.SafeTeamDefaultTemplate("support") != "operator" {
		t.Fatal("mode save clobbered policy")
	}
	if err := cfg.SetProjectSafeTeamPolicy("support", false, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.SafeTeamMode("support") {
		t.Fatal("want off after clear")
	}
	if err := cfg.SetProjectSafeTeamPolicy("support", true, "not-a-tpl"); err == nil {
		t.Fatal("want error for bad template")
	}
	if err := cfg.SetProjectDefaultMode("support", "nope"); err == nil {
		t.Fatal("want error for bad defaultMode")
	}
}

func TestProjectDirectToPrimary(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"solo": {Path: filepath.Join(dir, "solo")},
			"team": {Path: filepath.Join(dir, "team")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if cfg.ProjectDirectToPrimary("solo") {
		t.Fatal("default should be false")
	}
	if err := cfg.SetProjectDirectToPrimary("solo", true); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectDirectToPrimary("solo") {
		t.Fatal("want true after set")
	}
	if cfg.ProjectDirectToPrimary("team") {
		t.Fatal("team should stay false")
	}
	// Disk + marshal round-trip.
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Projects ProjectsMap `json:"projects"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Projects["solo"].DirectToPrimary == nil || !*disk.Projects["solo"].DirectToPrimary {
		t.Fatalf("disk lost flag: %+v", disk.Projects["solo"])
	}
	cloned := cloneProjectsMap(cfg.Projects)
	if cloned["solo"].DirectToPrimary == nil || !*cloned["solo"].DirectToPrimary {
		t.Fatalf("clone lost flag: %+v", cloned["solo"])
	}
	snap := cfg.Snapshot()
	var soloItem *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "solo" {
			soloItem = &snap.Projects[i]
			break
		}
	}
	if soloItem == nil || !soloItem.DirectToPrimary {
		t.Fatalf("snapshot want true: %+v", soloItem)
	}
	if err := cfg.SetProjectDirectToPrimary("solo", false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectDirectToPrimary("solo") {
		t.Fatal("want false after clear")
	}
	if err := cfg.SetProjectDirectToPrimary("missing", true); err == nil {
		t.Fatal("want error for missing project")
	}
}

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

func TestProjectClickUpAccessorsAndEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path: filepath.Join(dir, "app"),
				ClickUp: &ProjectClickUpConfig{
					Enabled:        true,
					WorkspaceID:    "ws1",
					ListID:         "list1",
					CustomIdPrefix: "DEV",
				},
			},
			"plain": {Path: filepath.Join(dir, "p")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !cfg.ProjectClickUpEnabled("app") {
		t.Fatal("expected enabled")
	}
	if cfg.ProjectClickUpEnabled("plain") {
		t.Fatal("plain should be off")
	}
	if cfg.ProjectClickUpCustomIdPrefix("app") != "DEV" {
		t.Fatal(cfg.ProjectClickUpCustomIdPrefix("app"))
	}
	if !cfg.ProjectClickUpPrefixParseEnabled("app") {
		t.Fatal("prefix parse should be on")
	}
	t.Setenv("CLICKUP_API_KEY_APP", "from-env")
	if got := cfg.ProjectClickUpAPIKey("app"); got != "from-env" {
		t.Fatalf("env key=%q", got)
	}
	if err := cfg.SetProjectClickUp("app", true, "ws2", "list2", "dev", "from-config", false); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectClickUpAPIKey("app"); got != "from-config" {
		t.Fatalf("config wins: %q", got)
	}
	if cfg.ProjectClickUpWorkspaceID("app") != "ws2" || cfg.ProjectClickUpListID("app") != "list2" {
		t.Fatal("ids")
	}
	if cfg.ProjectClickUpCustomIdPrefix("app") != "DEV" {
		t.Fatal("prefix upper")
	}
	// Collision with Linear team key disables prefix parse.
	if err := cfg.SetProjectLinear("app", true, "DEV", "lk", false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectClickUpPrefixParseEnabled("app") {
		t.Fatal("prefix parse should disable when == linear teamKey")
	}
}
