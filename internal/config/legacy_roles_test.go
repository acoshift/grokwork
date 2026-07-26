package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLegacyRoleGrantProjects scans raw config bytes, because after the fields
// were removed there is nothing left in the parsed Config to inspect.
func TestLegacyRoleGrantProjects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "allowedRoleIds only",
			raw:  `{"projects":{"p":{"path":"/p","allowedRoleIds":["r1"]}}}`,
			want: []string{"p"},
		},
		{
			name: "capabilityByRole only",
			raw:  `{"projects":{"p":{"path":"/p","capabilityByRole":{"r1":"builder"}}}}`,
			want: []string{"p"},
		},
		{
			name: "sorted, and string-form siblings are skipped not fatal",
			raw:  `{"projects":{"z":{"path":"/z","allowedRoleIds":["r"]},"a":"/a","m":{"path":"/m","capabilityByRole":{"r":"admin"}}}}`,
			want: []string{"m", "z"},
		},
		{
			name: "empty arrays and maps are not a grant",
			raw:  `{"projects":{"p":{"path":"/p","allowedRoleIds":[],"capabilityByRole":{}}}}`,
			want: nil,
		},
		{
			name: "teams config warns about nothing",
			raw:  `{"projects":{"p":{"path":"/p","teams":{"eng":{"members":["discord:1"]}}}}}`,
			want: nil,
		},
		{
			name: "no projects key",
			raw:  `{"discordToken":"t"}`,
			want: nil,
		},
		{
			name: "unparseable json is the real parse's problem",
			raw:  `{`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyRoleGrantProjects([]byte(tc.raw))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadFailsClosedOnRoleOnlyProject is the migration contract: a project whose
// only grant was a Discord role becomes inaccessible (correct), and Load says so
// loudly on stderr so the operator is not left guessing.
func TestLoadFailsClosedOnRoleOnlyProject(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"roleonly", "stringform", "teamed"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
  "discordToken": "test-token",
  "projects": {
    "roleonly": {
      "path": "` + filepath.Join(dir, "roleonly") + `",
      "allowedRoleIds": ["role-9"],
      "capabilityByRole": {"role-9": "builder"}
    },
    "stringform": "` + filepath.Join(dir, "stringform") + `",
    "teamed": {
      "path": "` + filepath.Join(dir, "teamed") + `",
      "teams": {"eng": {"members": ["discord:456"], "capabilities": "builder"}}
    }
  },
  "channels": {"c1": "roleonly", "c2": "teamed"}
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

	// The removed keys are gone and grant nothing.
	if cfg.ProjectHasAllowlist("roleonly") {
		t.Error("a role-only project must have no allowlist left")
	}
	if cfg.AccessAllowed("roleonly", "role-9") || cfg.AccessAllowed("roleonly", "anyone") {
		t.Error("a role-only project must be fail-closed after the migration")
	}
	// And the warning names it, exactly once, with the migration instruction.
	if n := strings.Count(warnings, `project "roleonly"`); n != 1 {
		t.Fatalf("want one warning naming roleonly, got %d in:\n%s", n, warnings)
	}
	if !strings.Contains(warnings, "allowedRoleIds") || !strings.Contains(warnings, "teams") {
		t.Errorf("warning does not tell the operator what to do:\n%s", warnings)
	}
	// A project written in string form must not have crashed the scanner, and a
	// teams-based project must neither warn nor lose its grant.
	if strings.Contains(warnings, "stringform") || strings.Contains(warnings, `project "teamed"`) {
		t.Errorf("warned about a project with no role grants:\n%s", warnings)
	}
	if !cfg.AccessAllowed("teamed", "456") {
		t.Error("teams-based access broke on the real Load path")
	}
	if !cfg.ResolveCapabilities("teamed", "456").CanShip() {
		t.Error("team capabilities broke on the real Load path")
	}
}

// captureStderr swaps os.Stderr for a pipe. The returned func restores it and
// returns everything written meanwhile.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	var out string
	var restored bool
	return func() string {
		if restored {
			return out
		}
		restored = true
		os.Stderr = orig
		_ = w.Close()
		out = <-done
		_ = r.Close()
		return out
	}
}
