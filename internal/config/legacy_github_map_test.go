package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLegacyGitHubIdentityIDs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"absent", `{"projects":{}}`, nil},
		{"empty", `{"discordUserGitHub":{}}`, nil},
		{
			"sorted",
			`{"discordUserGitHub":{"zed":{"login":"z"},"111":{"login":"a"},"aaa":{"login":"b"}}}`,
			[]string{"111", "aaa", "zed"},
		},
		// Unparseable JSON is the real parse's problem to report; this scanner
		// must not add a second confusing message about it.
		{"garbage", `{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyGitHubIdentityIDs([]byte(tc.raw)); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestLoadWarnsAboutDroppedGitHubMap is the migration contract for attribution.
//
// The key is gone and nothing reads it any more, so the people it used to cover
// silently stop appearing in commit trailers — the kind of loss nobody notices
// for weeks. Load has to name them, because this warning is the only signal the
// operator will ever get that somebody needs to go and link their GitHub login.
func TestLoadWarnsAboutDroppedGitHubMap(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
  "discordToken": "test-token",
  "projects": {"app": {"path": "` + proj + `", "allowedUserIds": ["111"]}},
  "channels": {"c1": "app"},
  "discordUserGitHub": {
    "222": {"login": "bob", "name": "Bob"},
    "111": {"login": "alice"}
  }
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
	if cfg == nil {
		t.Fatal("no config")
	}
	// Both ids are named: "2 mappings dropped" is not something an operator can act on.
	for _, id := range []string{"111", "222"} {
		if !strings.Contains(warnings, id) {
			t.Fatalf("warning does not name dropped user %q:\n%s", id, warnings)
		}
	}
	if !strings.Contains(warnings, "discordUserGitHub") || !strings.Contains(warnings, "/account") {
		t.Fatalf("warning does not tell the operator what to do:\n%s", warnings)
	}
	// The GitHub logins are an admin's unproven guess at somebody's handle, and
	// repeating them is exactly what this change exists to stop.
	for _, login := range []string{"alice", "bob"} {
		if strings.Contains(warnings, login) {
			t.Fatalf("warning echoed an unproven handle %q:\n%s", login, warnings)
		}
	}
}

// A config with no legacy map must keep its stderr byte-identical: a warning
// every operator sees on every boot is a warning nobody reads.
func TestLoadSaysNothingWithoutLegacyGitHubMap(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
  "discordToken": "test-token",
  "projects": {"app": {"path": "` + proj + `", "allowedUserIds": ["111"]}},
  "channels": {"c1": "app"}
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "")

	restore := captureStderr(t)
	if _, err := Load(); err != nil {
		restore()
		t.Fatalf("Load: %v", err)
	}
	if warnings := restore(); strings.Contains(warnings, "discordUserGitHub") {
		t.Fatalf("warned with no legacy map present:\n%s", warnings)
	}
}

// The key is not merely unread — it must not survive a round trip either. A
// config that still carried it after Save would keep resurrecting the warning
// forever, and would look to anyone reading config.json like a live setting.
func TestSaveDropsLegacyGitHubMap(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
  "discordToken": "tok",
  "projects": {"app": {"path": "` + dir + `"}},
  "channels": {},
  "discordUserGitHub": {"111": {"login": "alice"}}
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = cfgPath
	cfg.DataDir = dir
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "discordUserGitHub") {
		t.Fatalf("Save rewrote the removed key:\n%s", out)
	}
}
