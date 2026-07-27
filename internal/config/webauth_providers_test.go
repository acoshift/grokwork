package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearProviderEnv unsets every provider secret env var so a test's expectation
// cannot be satisfied by the developer's shell.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GOOGLE_CLIENT_SECRET", "GROK_WORK_GOOGLE_CLIENT_SECRET",
		"GITHUB_CLIENT_SECRET", "GROK_WORK_GITHUB_CLIENT_SECRET",
		"DISCORD_CLIENT_SECRET", "GROK_WORK_DISCORD_CLIENT_SECRET",
	} {
		t.Setenv(k, "")
	}
}

// TestOAuthProviderConfiguredNeedsBothHalves is the fail-closed gate: half a
// credential pair is not a provider, so neither a button nor a live callback
// route may follow from it.
func TestOAuthProviderConfiguredNeedsBothHalves(t *testing.T) {
	clearProviderEnv(t)
	cases := []struct {
		name   string
		google *OAuthProviderConfig
		want   bool
	}{
		{"both", &OAuthProviderConfig{ClientID: "gid", ClientSecret: "gsec"}, true},
		{"id only", &OAuthProviderConfig{ClientID: "gid"}, false},
		{"secret only", &OAuthProviderConfig{ClientSecret: "gsec"}, false},
		{"blank", &OAuthProviderConfig{}, false},
		{"whitespace only", &OAuthProviderConfig{ClientID: "  ", ClientSecret: " \t "}, false},
		{"nil block", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{WebAuth: &WebAuthConfig{
				Enabled:   true,
				Providers: &WebAuthProviders{Google: tc.google},
			}}
			if got := c.OAuthProviderConfigured(ActorKindGoogle); got != tc.want {
				t.Fatalf("OAuthProviderConfigured(google) = %v, want %v", got, tc.want)
			}
		})
	}

	// Absent providers block at all.
	bare := &Config{WebAuth: &WebAuthConfig{Enabled: true}}
	for _, kind := range []string{ActorKindGoogle, ActorKindGitHub} {
		if bare.OAuthProviderConfigured(kind) {
			t.Errorf("%s configured with no providers block", kind)
		}
	}
	if bare.AnyOAuthProviderConfigured() {
		t.Error("AnyOAuthProviderConfigured true with nothing configured")
	}

	// An unknown kind is never configured, and never panics.
	if bare.OAuthProviderConfigured("saml") {
		t.Error("unknown provider reported as configured")
	}
	if id, secret := bare.OAuthProviderCreds("saml"); id != "" || secret != "" {
		t.Errorf("unknown provider creds = (%q,%q), want empty", id, secret)
	}
	var nilCfg *Config
	if nilCfg.OAuthProviderConfigured(ActorKindGoogle) {
		t.Error("nil config reported a configured provider")
	}
}

// TestOAuthProviderCredsEnvFallback pins the secret-only env fallback and the
// precedence: the config value wins, env fills in, and nothing is guessed for
// the (public) client id.
func TestOAuthProviderCredsEnvFallback(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{WebAuth: &WebAuthConfig{
		Enabled: true,
		Providers: &WebAuthProviders{
			Google: &OAuthProviderConfig{ClientID: " gid ", ClientSecret: " from-config "},
			GitHub: &OAuthProviderConfig{ClientID: "hid"},
		},
	}}

	// Config wins over env.
	t.Setenv("GOOGLE_CLIENT_SECRET", "from-env")
	if got := c.GoogleClientSecretValue(); got != "from-config" {
		t.Fatalf("google secret = %q, want the config value to win (and be trimmed)", got)
	}
	if got := c.GoogleClientIDValue(); got != "gid" {
		t.Fatalf("google client id = %q, want trimmed %q", got, "gid")
	}

	// Env fills in when the config value is empty.
	if got := c.GitHubClientSecretValue(); got != "" {
		t.Fatalf("github secret = %q with no config and no env, want empty", got)
	}
	if c.OAuthProviderConfigured(ActorKindGitHub) {
		t.Fatal("github must not be configured on a client id alone")
	}
	t.Setenv("GITHUB_CLIENT_SECRET", "gh-from-env")
	if got := c.GitHubClientSecretValue(); got != "gh-from-env" {
		t.Fatalf("github secret = %q, want the env value", got)
	}
	if !c.OAuthProviderConfigured(ActorKindGitHub) {
		t.Fatal("github must be configured once the env secret is present")
	}

	// Product-prefixed name is the second choice, not the first.
	t.Setenv("GITHUB_CLIENT_SECRET", "")
	t.Setenv("GROK_WORK_GITHUB_CLIENT_SECRET", "gh-prefixed")
	if got := c.GitHubClientSecretValue(); got != "gh-prefixed" {
		t.Fatalf("github secret = %q, want the GROK_WORK_ fallback", got)
	}
	t.Setenv("GITHUB_CLIENT_SECRET", "gh-plain")
	if got := c.GitHubClientSecretValue(); got != "gh-plain" {
		t.Fatalf("github secret = %q, want the plain env name to win", got)
	}

	// The client id has no env fallback: it is public and belongs in config.
	t.Setenv("GOOGLE_CLIENT_ID", "id-from-env")
	empty := &Config{WebAuth: &WebAuthConfig{Enabled: true}}
	if got := empty.GoogleClientIDValue(); got != "" {
		t.Fatalf("google client id = %q, want no env fallback", got)
	}

	// OAuthProviderCreds routes by kind.
	id, secret := c.OAuthProviderCreds(ActorKindGoogle)
	if id != "gid" || secret != "from-config" {
		t.Fatalf("google creds = (%q,%q)", id, secret)
	}
	id, secret = c.OAuthProviderCreds("GitHub ")
	if id != "hid" || secret != "gh-plain" {
		t.Fatalf("github creds = (%q,%q) for a mixed-case kind", id, secret)
	}
}

// TestOAuthProviderCredsDiscord keeps Discord reachable through the same lookup
// the web layer uses for every other provider, including its bot-token-derived
// client id.
func TestOAuthProviderCredsDiscord(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{DiscordClientID: "111", DiscordClientSecret: "sec"}
	id, secret := c.OAuthProviderCreds(ActorKindDiscord)
	if id != "111" || secret != "sec" {
		t.Fatalf("discord creds = (%q,%q)", id, secret)
	}
	if !c.OAuthProviderConfigured(ActorKindDiscord) {
		t.Fatal("discord should be configured")
	}
	t.Setenv("DISCORD_CLIENT_SECRET", "env-sec")
	noSecret := &Config{DiscordClientID: "111"}
	if _, secret := noSecret.OAuthProviderCreds(ActorKindDiscord); secret != "env-sec" {
		t.Fatalf("discord secret = %q, want the env fallback", secret)
	}
}

// TestOAuthProviderKindsOrder pins the display order and the fact that the list
// is a copy — the web layer ranges over it to build the login page.
func TestOAuthProviderKindsOrder(t *testing.T) {
	got := OAuthProviderKinds()
	want := []string{"discord", "google", "github"}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	got[0] = "mutated"
	if OAuthProviderKinds()[0] != "discord" {
		t.Fatal("OAuthProviderKinds returned the backing slice; a caller can reorder the login page")
	}
}

// TestSaveLockedPreservesWebAuthProviders is the silent-data-loss guard:
// saveLocked marshals webAuth through cloneWebAuth, so a provider block missing
// from that clone is deleted from config.json the next time anyone saves from
// the web config UI.
func TestSaveLockedPreservesWebAuthProviders(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+dir+`", "allowedUserIds": ["u1"]}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`), 0o600); err != nil {
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
	cfg.WebAuth = &WebAuthConfig{
		Enabled:         true,
		AdminDiscordIDs: []string{"google:sub-1"},
		Providers: &WebAuthProviders{
			Google: &OAuthProviderConfig{ClientID: "gid", ClientSecret: "gsec"},
			GitHub: &OAuthProviderConfig{ClientID: "hid", ClientSecret: "hsec"},
		},
	}

	cfg.mu.Lock()
	err = cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw2, &again); err != nil {
		t.Fatal(err)
	}
	if again.WebAuth == nil || again.WebAuth.Providers == nil {
		t.Fatalf("webAuth.providers lost on save: %s", raw2)
	}
	if g := again.WebAuth.Providers.Google; g == nil || g.ClientID != "gid" || g.ClientSecret != "gsec" {
		t.Fatalf("google credentials lost: %+v", again.WebAuth.Providers.Google)
	}
	if h := again.WebAuth.Providers.GitHub; h == nil || h.ClientID != "hid" || h.ClientSecret != "hsec" {
		t.Fatalf("github credentials lost: %+v", again.WebAuth.Providers.GitHub)
	}

	// cloneWebAuth must deep-copy: a shared pointer lets a snapshot mutate config.
	clone := cloneWebAuth(cfg.WebAuth)
	if clone.Providers == cfg.WebAuth.Providers {
		t.Fatal("cloneWebAuth shared the providers block")
	}
	if clone.Providers.Google == cfg.WebAuth.Providers.Google {
		t.Fatal("cloneWebAuth shared a provider credential block")
	}
	clone.Providers.Google.ClientSecret = "tampered"
	if cfg.WebAuth.Providers.Google.ClientSecret != "gsec" {
		t.Fatal("mutating the clone changed the live config")
	}
}

// TestSaveKeepsDiscordOnlyWebAuthUnchanged: an existing Discord-only config must
// round-trip without growing an empty providers block.
func TestSaveKeepsDiscordOnlyWebAuthUnchanged(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+dir+`", "allowedUserIds": ["u1"]}},
  "channels": {"c1": "app"},
  "grokBin": "grok",
  "webAuth": {"enabled": true, "adminDiscordIds": ["111"]}
}
`), 0o600); err != nil {
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

	cfg.mu.Lock()
	err = cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "providers") {
		t.Fatalf("a Discord-only config grew a providers block: %s", out)
	}
}

func TestValidateWebAuthAcceptsGoogleOnly(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"google:sub-1"},
			Providers: &WebAuthProviders{
				Google: &OAuthProviderConfig{ClientID: "gid", ClientSecret: "gsec"},
			},
		},
	}
	if err := c.ValidateWebAuth(); err != nil {
		t.Fatalf("a Google-only deployment must validate without Discord OAuth: %v", err)
	}

	// GitHub-only, with the secret coming from env.
	t.Setenv("GITHUB_CLIENT_SECRET", "hsec")
	gh := &Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"github:42"},
			Providers: &WebAuthProviders{
				GitHub: &OAuthProviderConfig{ClientID: "hid"},
			},
		},
	}
	if err := gh.ValidateWebAuth(); err != nil {
		t.Fatalf("a GitHub-only deployment with an env secret must validate: %v", err)
	}
}

func TestValidateWebAuthRefusesWithNoProvider(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"111"},
		},
	}
	err := c.ValidateWebAuth()
	if err == nil {
		t.Fatal("webAuth enabled with zero login providers must not validate")
	}
	if !containsAll(err.Error(), "discordClientId", "discordClientSecret") {
		t.Fatalf("error should name the Discord fields a fresh config fills in: %v", err)
	}
}

// TestValidateWebAuthNamesHalfConfiguredProvider: once someone has clearly
// attempted a provider, the error must name that provider's missing half rather
// than sending them to Discord's fields.
func TestValidateWebAuthNamesHalfConfiguredProvider(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"google:sub-1"},
			Providers: &WebAuthProviders{
				Google: &OAuthProviderConfig{ClientID: "gid"},
			},
		},
	}
	err := c.ValidateWebAuth()
	if err == nil {
		t.Fatal("a client id with no secret is not a usable provider")
	}
	if !strings.Contains(err.Error(), "webAuth.providers.google.clientSecret") {
		t.Fatalf("error should name the missing Google secret: %v", err)
	}
	if strings.Contains(err.Error(), "discordClientId") {
		t.Fatalf("error should not fall back to Discord once a provider was attempted: %v", err)
	}

	// The mirrored half: a secret with no client id.
	c.WebAuth.Providers.Google = &OAuthProviderConfig{ClientSecret: "gsec"}
	err = c.ValidateWebAuth()
	if err == nil || !strings.Contains(err.Error(), "webAuth.providers.google.clientId") {
		t.Fatalf("error should name the missing Google client id: %v", err)
	}
}

// TestValidateWebAuthIgnoresStrayHalfWhenAnotherProviderWorks: a leftover field
// must not take down a deployment that is otherwise logging people in.
func TestValidateWebAuthIgnoresStrayHalfWhenAnotherProviderWorks(t *testing.T) {
	clearProviderEnv(t)
	c := &Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		WebAuth: &WebAuthConfig{
			Enabled:         true,
			AdminDiscordIDs: []string{"google:sub-1"},
			Providers: &WebAuthProviders{
				Google: &OAuthProviderConfig{ClientID: "gid", ClientSecret: "gsec"},
				GitHub: &OAuthProviderConfig{ClientID: "hid"}, // stray half
			},
		},
	}
	if err := c.ValidateWebAuth(); err != nil {
		t.Fatalf("a stray half-provider must warn, not fail: %v", err)
	}
	if c.OAuthProviderConfigured(ActorKindGitHub) {
		t.Fatal("the stray half must still not count as a provider")
	}
}

// TestLoadAcceptsProviderOnlyWebAuth is the end-to-end config path: a config
// with no Discord OAuth credentials at all boots when Google is configured, and
// the credentials survive Load.
func TestLoadAcceptsProviderOnlyWebAuth(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	projDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{
		"discordToken":     "test-token", // not decodable → no Discord client id
		"allowedUserIds":   []string{"u1"},
		"projects":         map[string]string{"p": projDir},
		"channels":         map[string]string{"c": "p"},
		"webPublicBaseURL": "http://127.0.0.1:8787",
		"webAuth": map[string]any{
			"enabled":         true,
			"adminDiscordIds": []string{"google:sub-1"},
			"providers": map[string]any{
				"google": map[string]any{"clientId": "gid", "clientSecret": "gsec"},
			},
		},
	})
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OAuthProviderConfigured(ActorKindGoogle) {
		t.Fatal("google provider lost through Load")
	}
	if cfg.OAuthProviderConfigured(ActorKindDiscord) {
		t.Fatal("discord must not be configured here")
	}
	role, ok := cfg.ResolveWebRoleForConfig("google:sub-1")
	if !ok || role != WebRoleAdmin {
		t.Fatalf("google admin role = (%q,%v), want (admin,true)", role, ok)
	}
	if _, ok := cfg.ResolveWebRoleForConfig("github:sub-1"); ok {
		t.Fatal("the same subject under github must not inherit the google admin grant")
	}
}
