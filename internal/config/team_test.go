package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestAccessAllowedByTeam is the core of the change: team membership is a grant.
func TestAccessAllowedByTeam(t *testing.T) {
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path: "/app",
			Teams: map[string]TeamConfig{
				"eng": {Label: "Engineering", Members: []string{"discord:456"}, Capabilities: "builder"},
			},
		},
	}}
	if !cfg.AccessAllowed("app", "456") {
		t.Error("a bare runtime id must match a namespaced team member")
	}
	if !cfg.AccessAllowed("app", "discord:456") {
		t.Error("the namespaced spelling must match too")
	}
	if cfg.AccessAllowed("app", "789") {
		t.Error("a non-member gained access")
	}
	if cfg.AccessAllowed("app", "") {
		t.Error("an empty actor id must never be granted access")
	}
	if cfg.AccessAllowed("nope", "456") {
		t.Error("unknown project must be fail-closed")
	}
	// A team written with bare ids (hand-edited config, never normalized) still works.
	bare := &Config{Projects: ProjectsMap{
		"app": {Teams: map[string]TeamConfig{"eng": {Members: []string{"456"}}}},
	}}
	if !bare.AccessAllowed("app", "discord:456") {
		t.Error("bare team member must match a namespaced probe")
	}
}

// TestProjectHasAllowlistIgnoresEmptyTeams pins the migration's fail-closed edge:
// a team stub with no members is not a grant, so a project whose role grants were
// dropped must not be re-opened by the leftover team declaration.
func TestProjectHasAllowlistIgnoresEmptyTeams(t *testing.T) {
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path:  "/app",
			Teams: map[string]TeamConfig{"eng": {Label: "Engineering", Capabilities: "builder"}},
		},
	}}
	if cfg.ProjectHasAllowlist("app") {
		t.Fatal("a team with no members must not satisfy the allowlist check")
	}
	if cfg.AccessAllowed("app", "456") {
		t.Fatal("empty-team project must be fail-closed")
	}
}

// TestResolveCapabilitiesByTeamOrsTemplates pins that capabilityByUser and every
// matching team OR together, exactly as capabilityByUser + capabilityByRole did.
func TestResolveCapabilitiesByTeamOrsTemplates(t *testing.T) {
	on := true
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path:             "/app",
			SafeTeamMode:     &on,
			CapabilityByUser: map[string]string{"discord:1": "operator"},
			Teams: map[string]TeamConfig{
				"support": {Members: []string{"discord:1", "discord:2"}, Capabilities: "investigator"},
				"eng":     {Members: []string{"discord:1"}, Capabilities: "builder"},
			},
		},
	}}
	// Actor 1: operator (byUser) + investigator (support) + builder (eng).
	caps := cfg.ResolveCapabilities("app", "1")
	if !caps.CanShip() {
		t.Errorf("builder team membership must contribute ship: %+v", caps)
	}
	if !caps.FileEscalation {
		t.Errorf("investigator team membership must contribute escalation: %+v", caps)
	}
	if !caps.DraftCustomerReply {
		t.Errorf("capabilityByUser must still contribute: %+v", caps)
	}
	if caps.AdminProject || caps.Merge {
		t.Errorf("OR of investigator/operator/builder must not grant admin: %+v", caps)
	}
	// Actor 2: only the support team.
	caps2 := cfg.ResolveCapabilities("app", "2")
	if caps2.CanShip() {
		t.Errorf("support-only member must not ship: %+v", caps2)
	}
	if !caps2.FileEscalation {
		t.Errorf("support member should escalate: %+v", caps2)
	}
}

// TestResolveCapabilitiesAccessOnlyTeamFallsBackToDefault: a team with no
// capabilities named grants access only, so its members land in the unmapped path.
func TestResolveCapabilitiesAccessOnlyTeamFallsBackToDefault(t *testing.T) {
	on := true
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path:         "/app",
			SafeTeamMode: &on,
			Teams:        map[string]TeamConfig{"guests": {Members: []string{"discord:9"}}},
		},
	}}
	caps := cfg.ResolveCapabilities("app", "9")
	if caps.CanShip() {
		t.Errorf("access-only team must not ship under safe team mode: %+v", caps)
	}
	if !caps.Investigate || !caps.FileEscalation {
		t.Errorf("want the investigator default: %+v", caps)
	}
}

// TestResolveCapabilitiesBrokenTemplateFailsClosed: a named-but-unresolvable
// capability template must not fall through to the *unmapped* default, which is
// builder with safeTeamMode off (its default) — a typo, or an operator deleting
// a capabilityTemplates overlay, would then promote a support team to
// builder-class. "Nothing named a template" and "the named template is broken"
// are different answers; only the first one gets a default.
func TestResolveCapabilitiesBrokenTemplateFailsClosed(t *testing.T) {
	zero := Capabilities{}
	on, off := true, false
	for _, tc := range []struct {
		name string
		pc   ProjectConfig
	}{
		{
			name: "team typo, safeTeamMode unset (default off → builder)",
			pc: ProjectConfig{
				Path: "/app",
				CapabilityTemplates: map[string]Capabilities{
					"support-l1": {Investigate: true},
				},
				Teams: map[string]TeamConfig{
					"support": {Members: []string{"discord:9"}, Capabilities: "support-l1-TYPO"},
				},
			},
		},
		{
			name: "team typo, safeTeamMode explicitly off",
			pc: ProjectConfig{
				Path:         "/app",
				SafeTeamMode: &off,
				Teams: map[string]TeamConfig{
					"support": {Members: []string{"discord:9"}, Capabilities: "buildr"},
				},
			},
		},
		{
			name: "team typo, safeTeamMode on",
			pc: ProjectConfig{
				Path:         "/app",
				SafeTeamMode: &on,
				Teams: map[string]TeamConfig{
					"support": {Members: []string{"discord:9"}, Capabilities: "buildr"},
				},
			},
		},
		{
			name: "capabilityByUser typo on a direct member",
			pc: ProjectConfig{
				Path:             "/app",
				AllowedUserIDs:   []string{"9"},
				CapabilityByUser: map[string]string{"9": "investigatr"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Projects: ProjectsMap{"app": tc.pc}}
			caps := cfg.ResolveCapabilities("app", "9")
			if caps != zero {
				t.Fatalf("a broken capability template must fail closed, got %+v", caps)
			}
			// Access is a separate grant: membership still holds, so the operator
			// sees a member who can do nothing rather than a silent promotion.
			if !cfg.AccessAllowed("app", "9") {
				t.Error("an unknown template must not revoke access")
			}
		})
	}

	// A second, *valid* membership is not punished for a broken sibling: whatever
	// resolves still applies (and nothing more).
	cfg := &Config{Projects: ProjectsMap{"app": {
		Path: "/app",
		Teams: map[string]TeamConfig{
			"support": {Members: []string{"discord:9"}, Capabilities: "investigator"},
			"eng":     {Members: []string{"discord:9"}, Capabilities: "buildr"},
		},
	}}}
	caps := cfg.ResolveCapabilities("app", "9")
	if !caps.FileEscalation {
		t.Errorf("the resolvable team must still apply: %+v", caps)
	}
	if caps.CanShip() {
		t.Errorf("the broken team must contribute nothing: %+v", caps)
	}

	// Control: with no template named anywhere, the unmapped default is unchanged.
	unmapped := &Config{Projects: ProjectsMap{"app": {
		Path:  "/app",
		Teams: map[string]TeamConfig{"eng": {Members: []string{"discord:9"}}},
	}}}
	if !unmapped.ResolveCapabilities("app", "9").CanShip() {
		t.Error("an access-only team with safeTeamMode off must still get the builder default")
	}
}

// TestLoadWarnsUnresolvedCapabilityTemplate: failing closed is right but silent,
// so Load names every template reference that resolves to nothing.
func TestLoadWarnsUnresolvedCapabilityTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
  "discordToken": "test-token",
  "projects": {
    "app": {
      "path": "` + filepath.Join(dir, "app") + `",
      "capabilityTemplates": {"support-l1": {"investigate": true}},
      "capabilityByUser": {"7": "operator"},
      "teams": {
        "support": {"members": ["discord:123"], "capabilities": "support-l1-TYPO"},
        "eng": {"members": ["discord:456"], "capabilities": "builder"}
      }
    }
  },
  "channels": {"c1": "app"}
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "")

	restore := captureStderr(t)
	cfg, err := Load()
	warnings := restore()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// normalizeTeams lowercases the template name on load, so the warning quotes
	// the stored (lowercased) spelling.
	if !strings.Contains(warnings, "support-l1-typo") ||
		!strings.Contains(warnings, "teams.support.capabilities") {
		t.Fatalf("no warning naming the broken team template:\n%s", warnings)
	}
	// The names that do resolve must not be reported.
	if strings.Contains(warnings, "teams.eng") || strings.Contains(warnings, "capabilityByUser.7") {
		t.Errorf("warned about a template that resolves:\n%s", warnings)
	}
	// And the runtime answer matches what the warning claims.
	if caps := cfg.ResolveCapabilities("app", "123"); caps != (Capabilities{}) {
		t.Errorf("broken team template granted %+v on the real Load path", caps)
	}
	if !cfg.ResolveCapabilities("app", "456").CanShip() {
		t.Error("the sibling team lost its grant")
	}
}

// TestResolveCapabilitiesTeamOverlayTemplate: a team may name a project overlay
// template, not just a builtin.
func TestResolveCapabilitiesTeamOverlayTemplate(t *testing.T) {
	on := true
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path:         "/app",
			SafeTeamMode: &on,
			CapabilityTemplates: map[string]Capabilities{
				"shipper": {Investigate: true, StartSessions: true, GithubWrites: true},
			},
			Teams: map[string]TeamConfig{"eng": {Members: []string{"discord:9"}, Capabilities: "shipper"}},
		},
	}}
	if !cfg.ResolveCapabilities("app", "9").CanShip() {
		t.Fatal("a project overlay template named by a team must resolve")
	}
}

// TestTeamOnlyMemberIsFullyVisible walks every membership reader at once. Each is
// an independent chance to forget teams, and forgetting one is invisible in the
// bot path — it shows up only as an empty web UI or a denied login.
func TestTeamOnlyMemberIsFullyVisible(t *testing.T) {
	cfg := &Config{
		WebAuth: &WebAuthConfig{Enabled: true},
		Projects: ProjectsMap{
			"app": {
				Path:  "/app",
				Teams: map[string]TeamConfig{"eng": {Members: []string{"oidc:alice"}, Capabilities: "builder"}},
			},
		},
	}
	if !cfg.AccessAllowed("app", "oidc:alice") {
		t.Error("AccessAllowed ignores teams")
	}
	if !cfg.UserOnAnyProject("oidc:alice") {
		t.Error("UserOnAnyProject ignores teams")
	}
	if !cfg.CanAccessProject("app", "oidc:alice", WebRoleMember) {
		t.Error("CanAccessProject ignores teams")
	}
	if got := cfg.ProjectsVisibleTo("oidc:alice", WebRoleMember); !slices.Equal(got, []string{"app"}) {
		t.Errorf("ProjectsVisibleTo = %v, want [app]", got)
	}
	role, ok := cfg.ResolveWebRoleForConfig("oidc:alice")
	if !ok || role != WebRoleMember {
		t.Errorf("ResolveWebRoleForConfig = (%q,%v), want (member,true) — a team-only member cannot log in", role, ok)
	}
	if !cfg.ResolveCapabilities("app", "oidc:alice").CanShip() {
		t.Error("ResolveCapabilities ignores teams")
	}
	// And a stranger is denied by every one of them.
	if cfg.AccessAllowed("app", "oidc:bob") || cfg.UserOnAnyProject("oidc:bob") ||
		cfg.CanAccessProject("app", "oidc:bob", WebRoleMember) ||
		len(cfg.ProjectsVisibleTo("oidc:bob", WebRoleMember)) != 0 {
		t.Error("a non-member passed a membership reader")
	}
	if _, ok := cfg.ResolveWebRoleForConfig("oidc:bob"); ok {
		t.Error("a non-member resolved a web role")
	}
}

func TestProjectsVisibleToHonoursTeams(t *testing.T) {
	cfg := &Config{Projects: ProjectsMap{
		"direct": {Path: "/d", AllowedUserIDs: []string{"u1"}},
		"team":   {Path: "/t", Teams: map[string]TeamConfig{"eng": {Members: []string{"discord:u1"}}}},
		"other":  {Path: "/o", AllowedUserIDs: []string{"u2"}},
	}}
	got := cfg.ProjectsVisibleTo("u1", WebRoleMember)
	if !slices.Equal(got, []string{"direct", "team"}) {
		t.Fatalf("visible = %v, want [direct team]", got)
	}
}

func TestWebAuthAdminAcceptsNamespacedID(t *testing.T) {
	cfg := &Config{
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"oidc:alice", "111222333444555666"},
		},
		Projects: ProjectsMap{"app": {Path: "/app"}},
	}
	for _, id := range []string{"oidc:alice", "111222333444555666", "discord:111222333444555666"} {
		role, ok := cfg.ResolveWebRoleForConfig(id)
		if !ok || role != WebRoleAdmin {
			t.Errorf("admin %q = (%q,%v), want (admin,true)", id, role, ok)
		}
	}
	if _, ok := cfg.ResolveWebRoleForConfig("oidc:bob"); ok {
		t.Error("a non-admin resolved through the admin list")
	}
	// A bare id must not match a differently-namespaced admin entry.
	if _, ok := cfg.ResolveWebRoleForConfig("alice"); ok {
		t.Error(`"alice" must not match "oidc:alice" — bare means Discord`)
	}
}

// TestProjectTeamsSurviveSaveLoad is the silent-data-loss guard: ProjectsMap keeps
// three hand-maintained copies of every field (UnmarshalJSON, MarshalJSON's
// outObj, cloneProjectsMap). If Teams is missing from any of them, the next
// unrelated project POST wipes every team with a success flash.
func TestProjectTeamsSurviveSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := &Config{
		ConfigPath: path,
		DataDir:    dir,
		Projects: ProjectsMap{
			"app": {
				Path: dir,
				Teams: map[string]TeamConfig{
					"eng": {Label: "Engineering", Members: []string{"discord:456", "oidc:alice"}, Capabilities: "builder"},
				},
			},
		},
	}
	// Mutate something entirely unrelated through a public setter so the real
	// saveLocked path rewrites the whole file.
	if err := cfg.SetProjectDefaultMode("app", "case"); err != nil {
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
	tm, ok := again.Projects["app"].Teams["eng"]
	if !ok {
		t.Fatalf("teams did not survive save/load: %s", raw)
	}
	if tm.Label != "Engineering" || tm.Capabilities != "builder" {
		t.Fatalf("team fields lost: %+v", tm)
	}
	if !slices.Equal(tm.Members, []string{"discord:456", "oidc:alice"}) {
		t.Fatalf("members lost: %v", tm.Members)
	}
	if !again.AccessAllowed("app", "456") {
		t.Fatal("reloaded config denies a team member")
	}

	// cloneProjectsMap is the other half: a shared Members backing array would let
	// a snapshot and the live config mutate each other.
	clone := cloneProjectsMap(cfg.Projects)
	clone["app"].Teams["eng"].Members[0] = "discord:hijacked"
	if cfg.Projects["app"].Teams["eng"].Members[0] != "discord:456" {
		t.Fatal("cloneProjectsMap shares the Members slice with the live config")
	}
}

// TestNormalizeTeamsOnLoad pins that keys, members and template names are
// canonicalized when config is parsed — the lookup paths assume it.
func TestNormalizeTeamsOnLoad(t *testing.T) {
	raw := []byte(`{"projects":{"app":{"path":"/app","teams":{" ENG ":{"label":" Engineering ","members":["456"," discord:456 ","oidc:alice",""],"capabilities":"BUILDER"},"":{"members":["1"]}}}}}`)
	var parsed struct {
		Projects ProjectsMap `json:"projects"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	teams := parsed.Projects["app"].Teams
	if len(teams) != 1 {
		t.Fatalf("want exactly the eng team (empty key dropped): %+v", teams)
	}
	tm, ok := teams["eng"]
	if !ok {
		t.Fatalf("team key not lowercased/trimmed: %+v", teams)
	}
	if tm.Label != "Engineering" {
		t.Errorf("label = %q", tm.Label)
	}
	if tm.Capabilities != "builder" {
		t.Errorf("capabilities = %q", tm.Capabilities)
	}
	if !slices.Equal(tm.Members, []string{"discord:456", "oidc:alice"}) {
		t.Errorf("members = %v, want normalized + deduped", tm.Members)
	}
}

func TestProjectTeamMutators(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ConfigPath: filepath.Join(dir, "config.json"),
		DataDir:    dir,
		Projects:   ProjectsMap{"app": {Path: dir}},
	}
	if err := cfg.SetProjectTeam("app", "Eng", "Engineering", "builder"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.ProjectTeams("app")["eng"]; !ok {
		t.Fatalf("team key not normalized: %+v", cfg.ProjectTeams("app"))
	}
	if err := cfg.SetProjectTeam("app", "eng", "Engineering", "not-a-template"); err == nil {
		t.Fatal("want an error for an unknown capability template")
	}
	if err := cfg.SetProjectTeam("app", "", "x", ""); err == nil {
		t.Fatal("want an error for an empty team key")
	}
	if err := cfg.SetProjectTeam("nope", "eng", "x", ""); err == nil {
		t.Fatal("want an error for an unknown project")
	}
	if err := cfg.AddProjectTeamMember("app", "eng", " 456 "); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same person under the other spelling is a no-op, not a dupe.
	if err := cfg.AddProjectTeamMember("app", "eng", "discord:456"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectTeams("app")["eng"].Members; !slices.Equal(got, []string{"discord:456"}) {
		t.Fatalf("members = %v, want one normalized entry", got)
	}
	if err := cfg.AddProjectTeamMember("app", "missing", "1"); err == nil {
		t.Fatal("want an error adding to a missing team")
	}
	// Updating label/capabilities keeps members.
	if err := cfg.SetProjectTeam("app", "eng", "Eng team", "approver"); err != nil {
		t.Fatal(err)
	}
	tm := cfg.ProjectTeams("app")["eng"]
	if tm.Label != "Eng team" || tm.Capabilities != "approver" || len(tm.Members) != 1 {
		t.Fatalf("update lost state: %+v", tm)
	}
	if err := cfg.AddProjectTeamMember("app", "eng", "oidc:alice"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectActorIDs("app"); !slices.Equal(got, []string{"discord:456", "oidc:alice"}) {
		t.Fatalf("ProjectActorIDs = %v", got)
	}
	if err := cfg.RemoveProjectTeamMember("app", "eng", "discord:999"); err == nil {
		t.Fatal("removing an absent member must be an error, not a silent success")
	}
	if err := cfg.RemoveProjectTeamMember("app", "eng", "456"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectTeams("app")["eng"].Members; !slices.Equal(got, []string{"oidc:alice"}) {
		t.Fatalf("after removal members = %v", got)
	}
	if err := cfg.RemoveProjectTeam("app", "missing"); err == nil {
		t.Fatal("removing an absent team must be an error")
	}
	if err := cfg.RemoveProjectTeam("app", "ENG"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProjectTeams("app")) != 0 {
		t.Fatalf("team survived removal: %+v", cfg.ProjectTeams("app"))
	}
	// Every mutator above went through saveLocked; the file must still parse.
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatalf("persisted config no longer parses: %v", err)
	}
}

func TestSnapshotTeamItems(t *testing.T) {
	cfg := &Config{Projects: ProjectsMap{
		"app": {
			Path:           "/app",
			AllowedUserIDs: []string{"u-direct"},
			Teams: map[string]TeamConfig{
				"support": {Members: []string{"discord:2"}, Capabilities: "investigator"},
				"eng":     {Label: "Engineering", Members: []string{"discord:1"}, Capabilities: "buildr"},
				"guests":  {Members: []string{"oidc:alice"}},
			},
		},
	}}
	item := cfg.Snapshot().Projects[0]
	if len(item.Teams) != 3 {
		t.Fatalf("teams = %+v", item.Teams)
	}
	// Sorted by key: eng, guests, support.
	if item.Teams[0].Key != "eng" || item.Teams[1].Key != "guests" || item.Teams[2].Key != "support" {
		t.Fatalf("teams not sorted by key: %+v", item.Teams)
	}
	if !item.Teams[0].TemplateUnknown {
		t.Error("a misspelled template must be flagged for the UI")
	}
	if item.Teams[2].TemplateUnknown {
		t.Error("investigator wrongly flagged unknown")
	}
	if item.Teams[1].Label != "guests" {
		t.Errorf("label should fall back to the key: %q", item.Teams[1].Label)
	}
	if item.Teams[0].Label != "Engineering" {
		t.Errorf("label = %q", item.Teams[0].Label)
	}
	want := []string{"discord:1", "discord:2", "discord:u-direct", "oidc:alice"}
	if !slices.Equal(item.MemberIDs, want) {
		t.Fatalf("MemberIDs = %v, want %v", item.MemberIDs, want)
	}
}

func TestActorSubject(t *testing.T) {
	for in, want := range map[string]string{
		"discord:123": "123",
		"123":         "123",
		"oidc:alice":  "alice",
		"weird:thing": "thing",
		"":            "",
		"discord:":    "",
	} {
		if got := ActorSubject(in); got != want {
			t.Errorf("ActorSubject(%q) = %q, want %q", in, got, want)
		}
	}
}
