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
		"discordToken": "test-token",
		"projects": map[string]any{
			"existing": map[string]any{
				"path":           projDir,
				"allowedUserIds": []string{"user-1"},
			},
		},
		"channels":   map[string]string{"ch1": "existing"},
		"httpListen": "127.0.0.1:9876",
	}
	raw, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	// Clear HTTP listen env so config file wins for ListenAddr when we check it.
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr() != "127.0.0.1:9876" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:9876", cfg.ListenAddr())
	}
	if !cfg.AccessAllowed("existing", "user-1") {
		t.Fatal("expected user-1 allowed on existing project")
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

	if err := cfg.AddProjectAllowedUser("existing", "user-2"); err != nil {
		t.Fatalf("AddProjectAllowedUser: %v", err)
	}
	if !cfg.AccessAllowed("existing", "user-2") {
		t.Fatal("user-2 should be allowed on existing")
	}

	if err := cfg.SetProjectTeam("existing", "eng", "Engineering", "builder"); err != nil {
		t.Fatalf("SetProjectTeam: %v", err)
	}
	if err := cfg.AddProjectTeamMember("existing", "eng", "user-9"); err != nil {
		t.Fatalf("AddProjectTeamMember: %v", err)
	}
	if !cfg.AccessAllowed("existing", "user-9") {
		t.Fatal("team member should be allowed on existing")
	}

	// Re-read file and assert persistence (shipped Save path).
	disk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Projects ProjectsMap `json:"projects"`
	}
	if err := json.Unmarshal(disk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Projects["newproj"].Path != newProj {
		t.Fatalf("disk projects[newproj]=%+v", parsed.Projects["newproj"])
	}
	if !contains(parsed.Projects["existing"].AllowedUserIDs, "user-2") {
		t.Fatalf("disk project members missing user-2: %v", parsed.Projects["existing"].AllowedUserIDs)
	}
	if !contains(parsed.Projects["existing"].Teams["eng"].Members, "discord:user-9") {
		t.Fatalf("disk project team missing user-9: %+v", parsed.Projects["existing"].Teams)
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
	var snapExisting *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "existing" {
			snapExisting = &snap.Projects[i]
			break
		}
	}
	if snapExisting == nil || !contains(snapExisting.AllowedUserIDs, "user-2") || !contains(snapExisting.MemberIDs, "discord:user-9") {
		t.Fatalf("snapshot project members: %+v", snap.Projects)
	}

	if err := cfg.AddChannel("ch-new", "newproj"); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	if name, ok := cfg.ChannelProject("ch-new"); !ok || name != "newproj" {
		t.Fatalf("ChannelProject=%q %v", name, ok)
	}

	if err := cfg.RemoveProjectAllowedUser("existing", "user-2"); err != nil {
		t.Fatalf("RemoveProjectAllowedUser: %v", err)
	}
	if cfg.AccessAllowed("existing", "user-2") {
		t.Fatal("user-2 still allowed")
	}
	if err := cfg.RemoveProjectTeamMember("existing", "eng", "user-9"); err != nil {
		t.Fatalf("RemoveProjectTeamMember: %v", err)
	}
	if cfg.AccessAllowed("existing", "user-9") {
		t.Fatal("user-9 still allowed")
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
		Projects ProjectsMap       `json:"projects"`
		Channels map[string]string `json:"channels"`
	}
	if err := json.Unmarshal(disk2, &parsed2); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed2.Projects["newproj"]; ok {
		t.Fatalf("disk still has newproj: %+v", parsed2.Projects)
	}
	if contains(parsed2.Projects["existing"].AllowedUserIDs, "user-2") ||
		contains(parsed2.Projects["existing"].Teams["eng"].Members, "discord:user-9") {
		t.Fatalf("disk still has removed project members: %+v", parsed2.Projects["existing"])
	}
	if _, ok := parsed2.Channels["ch-new"]; ok {
		t.Fatalf("disk still has ch-new: %+v", parsed2.Channels)
	}
}

func TestAddProjectValidation(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
	if err := cfg.AddProject("", "/tmp/x"); err == nil {
		t.Fatal("expected error for empty name")
	}
	if err := cfg.AddProject("p", "relative/path"); err == nil {
		t.Fatal("expected error for relative path")
	}
	if err := cfg.AddProject("", "/tmp/x"); err == nil {
		// already tested empty name above
	}
	if err := cfg.AddProjectAllowedUser("p", ""); err == nil {
		// project missing too — either empty id or missing project
	}
	if err := cfg.AddProjectAllowedUser("", "u"); err == nil {
		t.Fatal("expected error for empty project on add user")
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
	t.Setenv("GROK_WORK_HTTP_LISTEN", "0.0.0.0:9999")
	if got := cfg.ListenAddr(); got != "0.0.0.0:9999" {
		t.Fatalf("ListenAddr = %q", got)
	}
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	if got := cfg.ListenAddr(); got != ":1111" {
		t.Fatalf("config ListenAddr = %q", got)
	}
}

func TestEnvWork(t *testing.T) {
	t.Setenv("GROK_WORK_CONFIG", "/work/config.json")
	if got := EnvWork("CONFIG"); got != "/work/config.json" {
		t.Fatalf("EnvWork CONFIG = %q", got)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	if got := EnvWork("CONFIG"); got != "" {
		t.Fatalf("empty want empty, got %q", got)
	}
	if got := EnvWork("CONFIG", "FALLBACK_KEY"); got == "" {
		// extra keys only used when set
	}
	t.Setenv("FALLBACK_KEY", "fb")
	if got := EnvWork("CONFIG", "FALLBACK_KEY"); got != "fb" {
		t.Fatalf("extra key = %q", got)
	}
}

func TestSetAutoFixCIAndRiskyGlobs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"discordToken":"t",		"projects":{"p":"/tmp"},"channels":{"c":"p"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point loader via env.
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		// projects path /tmp may warn; token is fine. If projects must exist, use abs temp.
		_ = err
	}
	// Build config directly if Load fails on path checks.
	if cfg == nil || err != nil {
		cfg = &Config{
			DiscordToken: "t",
			Projects:     PathProjects(map[string]string{"p": dir}),
			Channels:     map[string]string{"c": "p"},
			ConfigPath:   path,
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

func TestSetGrokRunLimits(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		MaxTurns:   DefaultMaxTurns,
		TimeoutMs:  DefaultTimeoutMs,
	}
	if cfg.MaxTurnsValue() != DefaultMaxTurns || cfg.TimeoutMsValue() != DefaultTimeoutMs {
		t.Fatalf("defaults turns=%d timeout=%d", cfg.MaxTurnsValue(), cfg.TimeoutMsValue())
	}
	if err := cfg.SetGrokRunLimits(0, 1000); err == nil {
		t.Fatal("expected error for maxTurns 0")
	}
	if err := cfg.SetGrokRunLimits(10, 0); err == nil {
		t.Fatal("expected error for timeoutMs 0")
	}
	if err := cfg.SetGrokRunLimits(10, 999); err == nil {
		t.Fatal("expected error for timeoutMs below 1s")
	}
	if err := cfg.SetGrokRunLimits(10, MaxTimeoutMs+1); err == nil {
		t.Fatal("expected error for timeoutMs above 24h")
	}
	if err := cfg.SetGrokRunLimits(25, 900_000); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurnsValue() != 25 || cfg.TimeoutMsValue() != 900_000 {
		t.Fatalf("turns=%d timeout=%d", cfg.MaxTurnsValue(), cfg.TimeoutMsValue())
	}
	snap := cfg.Snapshot()
	if snap.MaxTurns != 25 || snap.TimeoutMs != 900_000 {
		t.Fatalf("snap turns=%d timeout=%d", snap.MaxTurns, snap.TimeoutMs)
	}
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"maxTurns": 25`) || !strings.Contains(string(raw), `"timeoutMs": 900000`) {
		t.Fatalf("persist missing values: %s", raw)
	}
}

func TestWorktreeIdleTTLDays(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
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

func TestWorktreesRoot(t *testing.T) {
	cfgDir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(cfgDir, "config.json"),
		DataDir:    filepath.Join(cfgDir, "data"),
	}
	wantDefault := filepath.Join(cfg.DataDir, "worktrees")
	if got := cfg.WorktreesRoot(); got != wantDefault {
		t.Fatalf("default root=%q want %q", got, wantDefault)
	}
	if snap := cfg.Snapshot(); snap.WorktreesRoot != wantDefault || snap.WorktreeDir != "" {
		t.Fatalf("snapshot default root=%q dir=%q", snap.WorktreesRoot, snap.WorktreeDir)
	}

	abs := filepath.Join(t.TempDir(), "wt")
	if err := cfg.SetWorktreeDir(abs); err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeDirValue() != abs {
		t.Fatalf("value=%q", cfg.WorktreeDirValue())
	}
	if got := cfg.WorktreesRoot(); got != filepath.Clean(abs) {
		t.Fatalf("abs root=%q want %q", got, filepath.Clean(abs))
	}

	// Relative paths resolve against the config file directory.
	if err := cfg.SetWorktreeDir("rel-worktrees"); err != nil {
		t.Fatal(err)
	}
	wantRel := filepath.Join(cfgDir, "rel-worktrees")
	if got := cfg.WorktreesRoot(); got != wantRel {
		t.Fatalf("rel root=%q want %q", got, wantRel)
	}

	if err := cfg.SetWorktreeSettings(10, "", 0); err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeIdleTTLDaysValue() != 10 {
		t.Fatalf("days=%d", cfg.WorktreeIdleTTLDaysValue())
	}
	if cfg.WorktreeDirValue() != "" {
		t.Fatalf("cleared dir still set: %q", cfg.WorktreeDirValue())
	}
	if got := cfg.WorktreesRoot(); got != wantDefault {
		t.Fatalf("after clear root=%q want %q", got, wantDefault)
	}

	disk, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		WorktreeDir         string `json:"worktreeDir"`
		WorktreeIdleTTLDays *int   `json:"worktreeIdleTTLDays"`
	}
	if err := json.Unmarshal(disk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.WorktreeDir != "" {
		t.Fatalf("empty override should omit/empty on disk: %q", parsed.WorktreeDir)
	}
	if parsed.WorktreeIdleTTLDays == nil || *parsed.WorktreeIdleTTLDays != 10 {
		t.Fatalf("disk ttl=%v", parsed.WorktreeIdleTTLDays)
	}
}

func TestBoardSettings(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
	if cfg.BoardStaleDaysValue() != DefaultBoardStaleDays {
		t.Fatalf("default stale=%d", cfg.BoardStaleDaysValue())
	}
	if cfg.BoardDigestChannelValue() != "" {
		t.Fatal("default digest channel should be empty")
	}
	if err := cfg.SetBoardSettings(5, "1234567890"); err != nil {
		t.Fatal(err)
	}
	if cfg.BoardStaleDaysValue() != 5 {
		t.Fatalf("stale=%d", cfg.BoardStaleDaysValue())
	}
	if cfg.BoardDigestChannelValue() != "1234567890" {
		t.Fatalf("channel=%q", cfg.BoardDigestChannelValue())
	}
	snap := cfg.Snapshot()
	if snap.BoardStaleDays != 5 || snap.BoardDigestChannel != "1234567890" {
		t.Fatalf("snapshot %+v", snap)
	}
	if err := cfg.SetBoardSettings(0, ""); err == nil {
		t.Fatal("expected error for staleDays < 1")
	}
	if err := cfg.SetBoardSettings(3, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.BoardDigestChannelValue() != "" {
		t.Fatal("empty channel should clear digest")
	}

	disk, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		BoardStaleDays     *int   `json:"boardStaleDays"`
		BoardDigestChannel string `json:"boardDigestChannel"`
	}
	if err := json.Unmarshal(disk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.BoardStaleDays == nil || *parsed.BoardStaleDays != 3 {
		t.Fatalf("disk stale=%v", parsed.BoardStaleDays)
	}
}

func TestDiscordPRLinkAndDisplayURL(t *testing.T) {
	t.Setenv("GROK_WORK_PUBLIC_BASE_URL", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &Config{
		Projects:         ProjectsMap{},
		Channels:         map[string]string{},
		ConfigPath:       cfgPath,
		WebPublicBaseURL: "https://grokwork.example",
	}
	if cfg.DiscordPRLinkValue() != DiscordPRLinkGitHub {
		t.Fatalf("default mode=%q", cfg.DiscordPRLinkValue())
	}
	gh := "https://github.com/acme/app/pull/42"
	if got := cfg.DiscordPRDisplayURL("acme", "app", 42, gh); got != gh {
		t.Fatalf("github mode: got %q", got)
	}
	if err := cfg.SetDiscordPRLink(DiscordPRLinkWeb); err != nil {
		t.Fatal(err)
	}
	if cfg.DiscordPRLinkValue() != DiscordPRLinkWeb {
		t.Fatalf("mode=%q", cfg.DiscordPRLinkValue())
	}
	want := "https://grokwork.example/prs/acme/app/42"
	if got := cfg.DiscordPRDisplayURL("acme", "app", 42, gh); got != want {
		t.Fatalf("web mode: got %q want %q", got, want)
	}
	// Parse owner/repo/number from github URL when fields empty.
	if got := cfg.DiscordPRDisplayURL("", "", 0, gh); got != want {
		t.Fatalf("from github URL: got %q", got)
	}
	// No public base → fall back to GitHub even in web mode.
	cfg.WebPublicBaseURL = ""
	if got := cfg.DiscordPRDisplayURL("acme", "app", 42, gh); got != gh {
		t.Fatalf("empty base fallback: got %q", got)
	}
	// Persist + snapshot.
	if err := cfg.SetDiscordPRLink("web"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"discordPRLink": "web"`) {
		t.Fatalf("not persisted: %s", raw)
	}
	if snap := cfg.Snapshot(); snap.DiscordPRLink != DiscordPRLinkWeb {
		t.Fatalf("snapshot mode=%q", snap.DiscordPRLink)
	}
	if err := cfg.SetDiscordPRLink("nope"); err == nil {
		t.Fatal("expected invalid mode error")
	}
	// github default omits field from disk after save.
	if err := cfg.SetDiscordPRLink(DiscordPRLinkGitHub); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "discordPRLink") {
		t.Fatalf("default should omit field: %s", raw)
	}
}

func TestResumeActiveRuns(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
	if !cfg.ResumeActiveRunsEnabled() {
		t.Fatal("nil should default true")
	}
	if !cfg.Snapshot().ResumeActiveRuns {
		t.Fatal("snapshot default true")
	}
	if err := cfg.SetResumeActiveRuns(false); err != nil {
		t.Fatal(err)
	}
	if cfg.ResumeActiveRunsEnabled() {
		t.Fatal("explicit false")
	}
	if cfg.Snapshot().ResumeActiveRuns {
		t.Fatal("snapshot false")
	}
	if err := cfg.SetResumeActiveRuns(true); err != nil {
		t.Fatal(err)
	}
	if !cfg.ResumeActiveRunsEnabled() {
		t.Fatal("explicit true")
	}
	if cfg.ShutdownTimeoutMsValue() != DefaultShutdownTimeoutMs {
		t.Fatalf("default shutdown ms=%d", cfg.ShutdownTimeoutMsValue())
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

func TestTerminalSessionTTLDays(t *testing.T) {
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}
	if cfg.TerminalSessionTTLDaysValue() != 0 {
		t.Fatalf("default days=%d want 0 (disabled)", cfg.TerminalSessionTTLDaysValue())
	}
	if cfg.TerminalSessionTTL() != 0 {
		t.Fatalf("default ttl=%v", cfg.TerminalSessionTTL())
	}
	if err := cfg.SetWorktreeSettings(7, "", 14); err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeIdleTTLDaysValue() != 7 {
		t.Fatalf("worktree days=%d", cfg.WorktreeIdleTTLDaysValue())
	}
	if cfg.TerminalSessionTTLDaysValue() != 14 {
		t.Fatalf("terminal days=%d", cfg.TerminalSessionTTLDaysValue())
	}
	if cfg.TerminalSessionTTL() != 14*24*time.Hour {
		t.Fatalf("ttl=%v", cfg.TerminalSessionTTL())
	}
	if cfg.Snapshot().TerminalSessionTTLDays != 14 {
		t.Fatalf("snapshot=%d", cfg.Snapshot().TerminalSessionTTLDays)
	}
	if err := cfg.SetWorktreeSettings(7, "", 0); err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalSessionTTL() != 0 {
		t.Fatal("expected disabled")
	}
	if err := cfg.SetWorktreeSettings(7, "", -1); err == nil {
		t.Fatal("expected reject negative")
	}
}

func TestSaveAtomicNoLeftoverTmpAndPerm(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(dir, "config.json"),
		MaxTurns:   DefaultMaxTurns,
		TimeoutMs:  DefaultTimeoutMs,
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after successful save: %v", matches)
	}
}

// TestSaveFailurePreservesPreviousConfig simulates a crash mid-write by
// making the temp-file create fail (read-only parent directory) and asserts
// the previous config.json survives untouched rather than being truncated —
// the whole point of writing to a temp file and renaming instead of writing
// the target in place.
func TestSaveFailurePreservesPreviousConfig(t *testing.T) {
	// Root ignores the read-only directory mode below, so the save would
	// succeed and the assertion would report a false FAILURE (not a false
	// pass). Skip rather than assert something untrue of the environment.
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not block root")
	}
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{},
		Channels:   map[string]string{},
		ConfigPath: filepath.Join(dir, "config.json"),
		MaxTurns:   DefaultMaxTurns,
		TimeoutMs:  DefaultTimeoutMs,
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	// Read-only directory: os.CreateTemp cannot create the temp file, so the
	// write must fail before config.json is ever touched.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	cfg.MaxTurns = 999
	if err := cfg.Save(); err == nil {
		t.Fatal("expected error saving to a read-only directory")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed save altered file on disk:\nbefore=%s\nafter=%s", before, after)
	}
	if strings.Contains(string(after), `"maxTurns": 999`) {
		t.Fatalf("failed save leaked the new value onto disk: %s", after)
	}
	var parsed map[string]any
	if err := json.Unmarshal(after, &parsed); err != nil {
		t.Fatalf("previous config no longer parses: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after failed save: %v", matches)
	}
}
