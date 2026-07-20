package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWebRoleOrder(t *testing.T) {
	in := RoleResolveInput{
		AdminIDs:       []string{"admin-1"},
		MemberIDs:      []string{"member-1", "admin-1"}, // admin still wins
		ViewerIDs:      []string{"viewer-1"},
		ProjectUserIDs: []string{"allow-1"},
	}
	cases := []struct {
		id   string
		role WebRole
		ok   bool
	}{
		{"admin-1", WebRoleAdmin, true},
		{"member-1", WebRoleMember, true},
		{"viewer-1", WebRoleViewer, true},
		{"allow-1", WebRoleMember, true},
		{"unknown", WebRoleNone, false},
		{"", WebRoleNone, false},
	}
	for _, tc := range cases {
		role, ok := ResolveWebRole(tc.id, in)
		if role != tc.role || ok != tc.ok {
			t.Fatalf("id=%q got (%q,%v) want (%q,%v)", tc.id, role, ok, tc.role, tc.ok)
		}
	}
}

func TestResolveWebRoleAllowedUserSet(t *testing.T) {
	role, ok := ResolveWebRole("u9", RoleResolveInput{
		ProjectUserSet: map[string]struct{}{"u9": {}},
	})
	if !ok || role != WebRoleMember {
		t.Fatalf("got %q %v", role, ok)
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !RoleAtLeast(WebRoleAdmin, WebRoleMember) {
		t.Fatal("admin should satisfy member")
	}
	if RoleAtLeast(WebRoleViewer, WebRoleMember) {
		t.Fatal("viewer should not satisfy member")
	}
	if !RoleAtLeast(WebRoleMember, WebRoleViewer) {
		t.Fatal("member should satisfy viewer")
	}
}

func TestWebAuthDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{
		"discordToken":   "test-token",
		"allowedUserIds": []string{"u1"},
		"projects":       map[string]string{"p": projDir},
		"channels":       map[string]string{"c": "p"},
	})
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebAuthEnabled() {
		t.Fatal("web auth should be disabled by default")
	}
	if err := cfg.ValidateWebAuth(); err != nil {
		t.Fatalf("ValidateWebAuth disabled: %v", err)
	}
	if cfg.Snapshot().WebAuthEnabled {
		t.Fatal("snapshot should show auth off")
	}
}

func TestWebAuthEnabledRequiresFields(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{
		"discordToken":    "test-token",
		"discordClientId": "111",
		"allowedUserIds":  []string{"u1"},
		"projects":        map[string]string{"p": projDir},
		"channels":        map[string]string{"c": "p"},
		"webAuth":         map[string]any{"enabled": true},
	})
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("DISCORD_CLIENT_SECRET", "")
	t.Setenv("GROK_WORK_DISCORD_CLIENT_SECRET", "")
	t.Setenv("GROK_WORK_SESSION_SECRET", "")
	t.Setenv("GROK_WORK_PUBLIC_BASE_URL", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail when webAuth enabled without secrets")
	}
	if !containsAll(err.Error(), "discordClientSecret", "webPublicBaseURL", "adminDiscordIds") {
		t.Fatalf("error should list missing fields: %v", err)
	}
	if strings.Contains(err.Error(), "sessionSecret") {
		t.Fatalf("sessionSecret should be optional: %v", err)
	}
}

func TestWebAuthEnabledWithBootstrapEnv(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{
		"discordToken":         "test-token",
		"discordClientId":      "424242424242424242",
		"discordClientSecret":  "sec",
		"webPublicBaseURL":     "http://127.0.0.1:8787",
		"allowedUserIds":       []string{"u1"},
		"projects":             map[string]string{"p": projDir},
		"channels":             map[string]string{"c": "p"},
		"webAuth": map[string]any{
			"enabled":       true,
			"sessionSecret": "sess-secret-32chars-minimum!!!!",
		},
	})
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", "")
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "admin-bootstrap")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WebAuthEnabled() {
		t.Fatal("want enabled")
	}
	admins := cfg.WebAuthAdminIDs()
	if len(admins) != 1 || admins[0] != "admin-bootstrap" {
		t.Fatalf("admins=%v", admins)
	}
	role, ok := cfg.ResolveWebRoleForConfig("admin-bootstrap")
	if !ok || role != WebRoleAdmin {
		t.Fatalf("bootstrap admin role=%q ok=%v", role, ok)
	}
	// Persist round-trip keeps webAuth.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(disk), `"enabled": true`, "webPublicBaseURL") {
		t.Fatalf("save missing webAuth fields: %s", disk)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
