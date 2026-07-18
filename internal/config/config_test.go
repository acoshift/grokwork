package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddProjectUserRolePersistAndRuntime(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	initial := map[string]any{
		"discordToken":   "test-token",
		"allowedUserIds": []string{"user-1"},
		"allowedRoleIds": []string{},
		"projects":       map[string]string{"existing": projDir},
		"channels":       map[string]string{"ch1": "existing"},
		"httpListen":     "127.0.0.1:9876",
	}
	raw, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GROK_DISCORD_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	// Clear HTTP listen env so config file wins for ListenAddr when we check it.
	t.Setenv("GROK_DISCORD_HTTP_LISTEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr() != "127.0.0.1:9876" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:9876", cfg.ListenAddr())
	}
	if !cfg.UserAllowed("user-1") {
		t.Fatal("expected user-1 allowed after load")
	}

	newProj := filepath.Join(dir, "newproj")
	if err := os.MkdirAll(newProj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProject("newproj", newProj); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if path, ok := cfg.ProjectPath("newproj"); !ok || path != newProj {
		t.Fatalf("ProjectPath newproj = %q,%v", path, ok)
	}

	if err := cfg.AddAllowedUser("user-2"); err != nil {
		t.Fatalf("AddAllowedUser: %v", err)
	}
	if !cfg.UserAllowed("user-2") {
		t.Fatal("user-2 should be allowed in runtime")
	}

	if err := cfg.AddAllowedRole("role-9"); err != nil {
		t.Fatalf("AddAllowedRole: %v", err)
	}
	if !cfg.RoleAllowed("role-9") {
		t.Fatal("role-9 should be allowed in runtime")
	}

	// Re-read file and assert persistence (shipped Save path).
	disk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Projects       map[string]string `json:"projects"`
		AllowedUserIDs []string          `json:"allowedUserIds"`
		AllowedRoleIDs []string          `json:"allowedRoleIds"`
	}
	if err := json.Unmarshal(disk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Projects["newproj"] != newProj {
		t.Fatalf("disk projects[newproj]=%q", parsed.Projects["newproj"])
	}
	if !contains(parsed.AllowedUserIDs, "user-2") {
		t.Fatalf("disk allowedUserIds missing user-2: %v", parsed.AllowedUserIDs)
	}
	if !contains(parsed.AllowedRoleIDs, "role-9") {
		t.Fatalf("disk allowedRoleIds missing role-9: %v", parsed.AllowedRoleIDs)
	}

	snap := cfg.Snapshot()
	foundProj := false
	for _, p := range snap.Projects {
		if p.Name == "newproj" && p.Path == newProj {
			foundProj = true
		}
	}
	if !foundProj {
		t.Fatalf("snapshot projects: %+v", snap.Projects)
	}
	if !contains(snap.AllowedUserIDs, "user-2") || !contains(snap.AllowedRoleIDs, "role-9") {
		t.Fatalf("snapshot allowlists: users=%v roles=%v", snap.AllowedUserIDs, snap.AllowedRoleIDs)
	}

	if err := cfg.AddChannel("ch-new", "newproj"); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	if name, ok := cfg.ChannelProject("ch-new"); !ok || name != "newproj" {
		t.Fatalf("ChannelProject=%q %v", name, ok)
	}

	if err := cfg.RemoveAllowedUser("user-2"); err != nil {
		t.Fatalf("RemoveAllowedUser: %v", err)
	}
	if cfg.UserAllowed("user-2") {
		t.Fatal("user-2 still allowed")
	}
	if err := cfg.RemoveAllowedRole("role-9"); err != nil {
		t.Fatalf("RemoveAllowedRole: %v", err)
	}
	if cfg.RoleAllowed("role-9") {
		t.Fatal("role-9 still allowed")
	}
	if err := cfg.RemoveChannel("ch-new"); err != nil {
		t.Fatalf("RemoveChannel: %v", err)
	}
	if _, ok := cfg.ChannelProject("ch-new"); ok {
		t.Fatal("ch-new still mapped")
	}

	// Removing project cascades channel maps that point to it.
	if err := cfg.AddChannel("ch1b", "newproj"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RemoveProject("newproj"); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if _, ok := cfg.ProjectPath("newproj"); ok {
		t.Fatal("newproj still present")
	}
	if _, ok := cfg.ChannelProject("ch1b"); ok {
		t.Fatal("cascaded channel still present")
	}

	disk2, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed2 struct {
		Projects       map[string]string `json:"projects"`
		Channels       map[string]string `json:"channels"`
		AllowedUserIDs []string          `json:"allowedUserIds"`
		AllowedRoleIDs []string          `json:"allowedRoleIds"`
	}
	if err := json.Unmarshal(disk2, &parsed2); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed2.Projects["newproj"]; ok {
		t.Fatalf("disk still has newproj: %+v", parsed2.Projects)
	}
	if contains(parsed2.AllowedUserIDs, "user-2") || contains(parsed2.AllowedRoleIDs, "role-9") {
		t.Fatalf("disk still has removed allowlist: %+v %+v", parsed2.AllowedUserIDs, parsed2.AllowedRoleIDs)
	}
	if _, ok := parsed2.Channels["ch-new"]; ok {
		t.Fatalf("disk still has ch-new: %+v", parsed2.Channels)
	}
}

func TestAddProjectValidation(t *testing.T) {
	cfg := &Config{
		Projects:     map[string]string{},
		Channels:     map[string]string{},
		AllowedUsers: map[string]struct{}{},
		AllowedRoles: map[string]struct{}{},
		ConfigPath:   filepath.Join(t.TempDir(), "config.json"),
	}
	if err := cfg.AddProject("", "/tmp/x"); err == nil {
		t.Fatal("expected error for empty name")
	}
	if err := cfg.AddProject("p", "relative/path"); err == nil {
		t.Fatal("expected error for relative path")
	}
	if err := cfg.AddAllowedUser(""); err == nil {
		t.Fatal("expected error for empty user")
	}
	if err := cfg.AddAllowedRole(""); err == nil {
		t.Fatal("expected error for empty role")
	}
	if err := cfg.AddChannel("ch", "missing"); err == nil {
		t.Fatal("expected error for unknown project")
	}
	if err := cfg.RemoveProject("nope"); err == nil {
		t.Fatal("expected error for missing project")
	}
}

func TestListenAddrEnvOverride(t *testing.T) {
	cfg := &Config{HTTPListen: ":1111"}
	t.Setenv("GROK_DISCORD_HTTP_LISTEN", "0.0.0.0:9999")
	if got := cfg.ListenAddr(); got != "0.0.0.0:9999" {
		t.Fatalf("ListenAddr = %q", got)
	}
}

func TestSetAutoFixCIAndRiskyGlobs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"discordToken":"t","allowedUserIds":["u"],
		"projects":{"p":"/tmp"},"channels":{"c":"p"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point loader via env.
	t.Setenv("GROK_DISCORD_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		// projects path /tmp may warn; token is fine. If projects must exist, use abs temp.
		_ = err
	}
	// Build config directly if Load fails on path checks.
	if cfg == nil || err != nil {
		cfg = &Config{
			DiscordToken:   "t",
			AllowedUserIDs: []string{"u"},
			Projects:       map[string]string{"p": dir},
			Channels:       map[string]string{"c": "p"},
			ConfigPath:     path,
			AllowedUsers:   map[string]struct{}{"u": {}},
		}
	}

	if cfg.AutoFixCIEnabled() {
		t.Fatal("default auto fix should be off")
	}
	if err := cfg.SetAutoFixCI(true, 0); err == nil {
		t.Fatal("expected error for max 0")
	}
	if err := cfg.SetAutoFixCI(true, 3); err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoFixCIEnabled() || cfg.AutoFixCIMaxAttempts() != 3 {
		t.Fatalf("auto=%v max=%d", cfg.AutoFixCIEnabled(), cfg.AutoFixCIMaxAttempts())
	}
	snap := cfg.Snapshot()
	if !snap.AutoFixCI || snap.AutoFixCIMax != 3 {
		t.Fatalf("snap=%+v", snap)
	}
	// Defaults mode still shows default patterns in the snapshot for the UI.
	if err := cfg.SetRiskyPathGlobsFromText("", true); err != nil {
		t.Fatal(err)
	}
	snap = cfg.Snapshot()
	if !snap.RiskyPathUseDefault || !strings.Contains(snap.RiskyPathGlobsText, "migrations") {
		t.Fatalf("default display snap=%+v", snap)
	}

	if err := cfg.SetRiskyPathGlobsFromText("**/auth/**\n# comment\n**/deploy/**", false); err != nil {
		t.Fatal(err)
	}
	if !cfg.RiskyPathGlobsConfigured() || len(cfg.RiskyPathGlobsEffective()) != 2 {
		t.Fatalf("globs=%v", cfg.RiskyPathGlobsEffective())
	}
	if err := cfg.SetRiskyPathGlobsFromText("", true); err != nil {
		t.Fatal(err)
	}
	if cfg.RiskyPathGlobsConfigured() {
		t.Fatal("expected defaults after useDefault")
	}
	if err := cfg.SetRiskyPathGlobsFromText("", false); err != nil {
		t.Fatal(err)
	}
	if !cfg.RiskyPathGlobsConfigured() || len(cfg.RiskyPathGlobsEffective()) != 0 {
		t.Fatalf("empty custom: %v", cfg.RiskyPathGlobsEffective())
	}
}

func TestWorktreeIdleTTLDays(t *testing.T) {
	cfg := &Config{
		Projects:   map[string]string{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
	if cfg.WorktreeIdleTTLDaysValue() != DefaultWorktreeIdleTTLDays {
		t.Fatalf("default days=%d", cfg.WorktreeIdleTTLDaysValue())
	}
	if cfg.WorktreeIdleTTL() != time.Duration(DefaultWorktreeIdleTTLDays)*24*time.Hour {
		t.Fatalf("default ttl=%v", cfg.WorktreeIdleTTL())
	}
	if err := cfg.SetWorktreeIdleTTLDays(7); err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeIdleTTLDaysValue() != 7 {
		t.Fatalf("days=%d", cfg.WorktreeIdleTTLDaysValue())
	}
	if cfg.Snapshot().WorktreeIdleTTLDays != 7 {
		t.Fatalf("snapshot days=%d", cfg.Snapshot().WorktreeIdleTTLDays)
	}
	if err := cfg.SetWorktreeIdleTTLDays(0); err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeIdleTTL() != 0 {
		t.Fatal("0 should disable")
	}
	if err := cfg.SetWorktreeIdleTTLDays(-1); err == nil {
		t.Fatal("expected error for negative")
	}

	disk, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		WorktreeIdleTTLDays *int `json:"worktreeIdleTTLDays"`
	}
	if err := json.Unmarshal(disk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.WorktreeIdleTTLDays == nil || *parsed.WorktreeIdleTTLDays != 0 {
		t.Fatalf("disk ttl=%v", parsed.WorktreeIdleTTLDays)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
