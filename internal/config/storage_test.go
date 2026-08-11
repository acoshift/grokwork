package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storageTestConfig(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "discordToken": "tok",
  "projects": {
    "app": {"path": "` + dir + `", "allowedUserIds": ["u1"]},
    "api": {"path": "` + dir + `", "allowedUserIds": ["u1"]}
  },
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

func TestProjectStorageRoundTrip(t *testing.T) {
	cfg, path := storageTestConfig(t)
	if err := cfg.SetProjectStorageGCS("app", "acme-app-files", "grokwork", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}

	// Unrelated save must not drop storage (MarshalJSON outObj + clone trap).
	if err := cfg.SetProjectVerifyCommands("app", []VerifyCommand{{Name: "t", Command: "true"}}); err != nil {
		t.Fatal(err)
	}

	again := reloadConfig(t, path)
	st := again.ProjectStorage("app")
	if st == nil {
		t.Fatal("storage lost after reload")
	}
	if st.GCSBucket != "acme-app-files" || st.Prefix != "grokwork" || st.CredentialsFile != "/etc/keys/svc.json" {
		t.Fatalf("storage = %+v", st)
	}

	// Snapshot projection (raw override).
	snap := again.Snapshot()
	var item *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "app" {
			item = &snap.Projects[i]
		}
	}
	if item == nil {
		t.Fatal("project missing from snapshot")
	}
	if item.StorageBucket != "acme-app-files" || item.StoragePrefix != "grokwork" || item.StorageCredentialsFile != "/etc/keys/svc.json" {
		t.Fatalf("snapshot storage = %q / %q / %q", item.StorageBucket, item.StoragePrefix, item.StorageCredentialsFile)
	}
	if item.StorageSource != StorageSourceOverride {
		t.Fatalf("StorageSource = %q, want override", item.StorageSource)
	}
	if item.StorageEffectiveBucket != "acme-app-files" || item.StorageEffectivePrefix != "grokwork" {
		t.Fatalf("effective = %q / %q", item.StorageEffectiveBucket, item.StorageEffectivePrefix)
	}

	// cloneProjectsMap detaches the pointer.
	cloned := cloneProjectsMap(again.Projects)
	if cloned["app"].Storage == again.Projects["app"].Storage {
		t.Fatal("cloneProjectsMap shares Storage pointer")
	}
	cloned["app"].Storage.GCSBucket = "mutated"
	if again.Projects["app"].Storage.GCSBucket == "mutated" {
		t.Fatal("clone mutation leaked into live config")
	}
}

func TestGlobalStorageRoundTrip(t *testing.T) {
	cfg, path := storageTestConfig(t)
	if err := cfg.SetGlobalStorageGCS("acme-company-files", "grokwork", "/etc/keys/g.json"); err != nil {
		t.Fatal(err)
	}
	// Unrelated mutator must not drop global storage (outObj whitelist trap).
	if err := cfg.SetProjectVerifyCommands("app", []VerifyCommand{{Name: "t", Command: "true"}}); err != nil {
		t.Fatal(err)
	}
	again := reloadConfig(t, path)
	st := again.GlobalStorage()
	if st == nil {
		t.Fatal("global storage lost after reload")
	}
	if st.GCSBucket != "acme-company-files" || st.Prefix != "grokwork" || st.CredentialsFile != "/etc/keys/g.json" {
		t.Fatalf("global = %+v", st)
	}
	snap := again.Snapshot()
	if !snap.StorageConfigured || snap.StorageBucket != "acme-company-files" || snap.StoragePrefix != "grokwork" {
		t.Fatalf("snapshot global = configured=%v %q/%q", snap.StorageConfigured, snap.StorageBucket, snap.StoragePrefix)
	}
	// Clone detaches.
	g1 := again.GlobalStorage()
	g2 := again.GlobalStorage()
	if g1 == g2 {
		t.Fatal("GlobalStorage returns same pointer")
	}
	g1.GCSBucket = "mutated"
	if again.GlobalStorage().GCSBucket == "mutated" {
		t.Fatal("mutation leaked")
	}
}

func TestSetProjectStorageGCSValidation(t *testing.T) {
	cfg, _ := storageTestConfig(t)

	cases := []struct {
		name   string
		bucket string
		prefix string
		creds  string
		ok     bool
	}{
		{"good", "acme-files", "prefix", "", true},
		{"good-dots", "acme.files-1", "", "", true},
		{"good-creds", "acme-files", "", "/etc/keys/svc.json", true},
		{"empty-bucket", "", "ignored", "", false},
		{"too-short", "ab", "", "", false},
		{"upper", "Acme-Files", "", "", false},
		{"leading-slash-prefix", "acme-files", "/bad", "", false},
		{"dotdot-prefix", "acme-files", "a/../b", "", false},
		{"wildcard-prefix", "acme-files", "foo*", "", false},
		{"wildcard-bucket", "acme*", "", "", false},
		{"control-prefix", "acme-files", "a\nb", "", false},
		{"relative-creds", "acme-files", "", "keys/svc.json", false},
		{"control-creds", "acme-files", "", "/etc/keys/a\nb.json", false},
		{"unknown-project", "acme-files", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proj := "app"
			if tc.name == "unknown-project" {
				proj = "nope"
			}
			err := cfg.SetProjectStorageGCS(proj, tc.bucket, tc.prefix, tc.creds)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestSetProjectStorageGCSRequiresBucket(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	if err := cfg.SetProjectStorageGCS("app", "acme-files", "p", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageGCS("app", "", "", ""); err == nil {
		t.Fatal("empty bucket must error, not clear")
	}
	if st := cfg.ProjectStorage("app"); st == nil || st.GCSBucket != "acme-files" {
		t.Fatalf("empty set mutated storage: %+v", st)
	}
}

func TestClearAndDisableProjectStorage(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	if err := cfg.SetGlobalStorageGCS("acme-company", "gw", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageGCS("app", "acme-files", "p", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ClearProjectStorage("app"); err != nil {
		t.Fatal(err)
	}
	if raw := cfg.ProjectStorage("app"); raw != nil {
		t.Fatalf("clear left raw: %+v", raw)
	}
	eff := cfg.EffectiveStorage("app")
	if eff == nil || eff.GCSBucket != "acme-company" || eff.Prefix != "gw/app" {
		t.Fatalf("clear under global: effective=%+v", eff)
	}

	if err := cfg.SetProjectStorageDisabled("app"); err != nil {
		t.Fatal(err)
	}
	raw := cfg.ProjectStorage("app")
	if raw == nil || !raw.Disabled {
		t.Fatalf("disable raw=%+v", raw)
	}
	if cfg.EffectiveStorage("app") != nil {
		t.Fatal("disabled must yield nil effective")
	}
	// clear after disable re-inherits
	if err := cfg.ClearProjectStorage("app"); err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveStorage("app") == nil {
		t.Fatal("clear after disable should re-inherit")
	}

	// Without global, clear unlinks
	if err := cfg.SetGlobalStorageGCS("", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageGCS("app", "acme-files", "p", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ClearProjectStorage("app"); err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveStorage("app") != nil {
		t.Fatal("clear without global should nil effective")
	}
}

func TestEffectiveStorageMatrix(t *testing.T) {
	cfg, _ := storageTestConfig(t)

	if cfg.EffectiveStorage("app") != nil {
		t.Fatal("no global, no project → nil")
	}
	if cfg.EffectiveStorage("nope") != nil {
		t.Fatal("unknown project → nil")
	}

	if err := cfg.SetGlobalStorageGCS("acme-company-files", "grokwork", "/etc/k.json"); err != nil {
		t.Fatal(err)
	}
	// api has no override → inherit joined
	eff := cfg.EffectiveStorage("api")
	if eff == nil || eff.GCSBucket != "acme-company-files" || eff.Prefix != "grokwork/api" || eff.CredentialsFile != "/etc/k.json" {
		t.Fatalf("inherit = %+v", eff)
	}
	// empty global prefix
	if err := cfg.SetGlobalStorageGCS("acme-company-files", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveStorage("api"); got == nil || got.Prefix != "api" {
		t.Fatalf("empty base prefix join = %+v", got)
	}
	if err := cfg.SetGlobalStorageGCS("acme-company-files", "pfx", ""); err != nil {
		t.Fatal(err)
	}

	// override as-is (no auto segment)
	if err := cfg.SetProjectStorageGCS("app", "acme-app-private", "prod", ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveStorage("app"); got == nil || got.GCSBucket != "acme-app-private" || got.Prefix != "prod" {
		t.Fatalf("override = %+v", got)
	}
	// raw does not fall back
	if cfg.ProjectStorage("api") != nil {
		t.Fatal("raw for inheriting project must be nil")
	}

	if err := cfg.SetProjectStorageDisabled("api"); err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveStorage("api") != nil {
		t.Fatal("disabled+global → nil")
	}
	snap := cfg.Snapshot()
	for _, p := range snap.Projects {
		if p.Name == "api" {
			if p.StorageSource != StorageSourceDisabled || !p.StorageDisabled {
				t.Fatalf("api snapshot source=%q disabled=%v", p.StorageSource, p.StorageDisabled)
			}
			if p.StorageBucket != "" || p.StorageEffectiveBucket != "" {
				t.Fatalf("disabled snapshot leaked bucket fields: %+v", p)
			}
		}
		if p.Name == "app" {
			if p.StorageSource != StorageSourceOverride || p.StorageInherited {
				t.Fatalf("app snapshot = source=%q inherited=%v", p.StorageSource, p.StorageInherited)
			}
		}
	}
}

func TestStorageProjectSegment(t *testing.T) {
	if got := storageProjectSegment("api"); got != "api" {
		t.Fatalf("passthrough = %q", got)
	}
	if got := storageProjectSegment("  app  "); got != "app" {
		t.Fatalf("trim = %q", got)
	}
	// empty-after-sanitize → hash, never ""
	got := storageProjectSegment("!!!")
	if got == "" || !strings.HasPrefix(got, "p_") || len(got) != 2+16 {
		t.Fatalf("!!! segment = %q", got)
	}
	sum := sha256.Sum256([]byte("!!!"))
	want := fmt.Sprintf("p_%x", sum[:8])
	if got != want {
		t.Fatalf("hash = %q want %q", got, want)
	}
	// unicode → hash or sanitize; never empty
	if got := storageProjectSegment("ไทย"); got == "" {
		t.Fatal("unicode segment empty")
	}
	// slash names sanitize
	if got := storageProjectSegment("a/b"); got == "a/b" || strings.Contains(got, "/") {
		t.Fatalf("slash segment = %q", got)
	}
	// pinned collision: foo!bar and foo_bar → same segment
	if a, b := storageProjectSegment("foo!bar"), storageProjectSegment("foo_bar"); a != b {
		t.Fatalf("collision pin: %q vs %q", a, b)
	}
	if a := storageProjectSegment("foo!bar"); a != "foo_bar" {
		t.Fatalf("sanitize = %q", a)
	}
	// charset post-condition
	for _, name := range []string{"api", "!!!", "foo!bar", "a/b", "...", "x"} {
		s := storageProjectSegment(name)
		if s == "" || !storageSegmentRe.MatchString(s) {
			t.Fatalf("segment %q for %q fails post-condition", s, name)
		}
	}
}

func TestJoinStoragePrefixValidate(t *testing.T) {
	got, err := JoinStoragePrefix("grokwork", "api")
	if err != nil || got != "grokwork/api" {
		t.Fatalf("join = %q err=%v", got, err)
	}
	got, err = JoinStoragePrefix("", "api")
	if err != nil || got != "api" {
		t.Fatalf("empty base = %q err=%v", got, err)
	}
	got, err = JoinStoragePrefix("team/files", "app")
	if err != nil || got != "team/files/app" {
		t.Fatalf("nested = %q err=%v", got, err)
	}
}

func TestLoadRejectsInvalidStoredBucket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "discordToken": "tok",
  "projects": {
    "app": {
      "path": "` + dir + `",
      "storage": {"gcsBucket": "BAD_BUCKET", "prefix": "x"}
    }
  },
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
	err = json.Unmarshal(raw, &cfg)
	if err == nil {
		t.Fatal("want load error for invalid stored bucket")
	}
	if !strings.Contains(err.Error(), "bucket") && !strings.Contains(err.Error(), "storage") {
		t.Fatalf("error should name the problem: %v", err)
	}
}

func TestLoadRejectsRelativeStoredCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "discordToken": "tok",
  "projects": {
    "app": {
      "path": "` + dir + `",
      "storage": {"gcsBucket": "acme-files", "credentialsFile": "keys/svc.json"}
    }
  },
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
	err = json.Unmarshal(raw, &cfg)
	if err == nil {
		t.Fatal("want load error for relative stored credentialsFile")
	}
	if !strings.Contains(err.Error(), "credentialsFile") {
		t.Fatalf("error should name the field: %v", err)
	}
}

func TestGlobalStorageRejectsDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "discordToken": "tok",
  "storage": {"disabled": true},
  "projects": {"app": {"path": "` + dir + `"}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Load() path (not bare Unmarshal) is where global normalize runs.
	// Simulate Load's post-unmarshal normalize:
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	_, err = normalizeStorage(cfg.Storage, false)
	if err == nil {
		t.Fatal("want error for disabled on global")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadNormalizesGlobalStorage(t *testing.T) {
	dir := t.TempDir()
	// invalid global bucket
	path := filepath.Join(dir, "bad.json")
	body := `{
  "discordToken": "tok",
  "storage": {"gcsBucket": "BAD"},
  "projects": {"app": {"path": "` + dir + `"}},
  "channels": {"c1": "app"},
  "grokBin": "grok"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeStorage(cfg.Storage, false); err == nil {
		t.Fatal("want invalid global bucket error")
	}

	// empty global object → nil
	empty := &StorageConfig{}
	got, err := normalizeStorage(empty, false)
	if err != nil || got != nil {
		t.Fatalf("empty global = %+v err=%v", got, err)
	}
}

func TestNormalizeStripsTrailingPrefixSlash(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	if err := cfg.SetProjectStorageGCS("app", "acme-files", "grokwork/", ""); err != nil {
		t.Fatal(err)
	}
	st := cfg.ProjectStorage("app")
	if st == nil || st.Prefix != "grokwork" {
		t.Fatalf("prefix = %+v, want grokwork without trailing slash", st)
	}
}

func TestFeatureStorageFailsClosedWithoutWebAuth(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	cfg.WebAuth = &WebAuthConfig{
		Enabled:  false,
		Features: WebAuthFeatures{Storage: true},
	}
	if cfg.FeatureStorage() {
		t.Fatal("FeatureStorage is true with webAuth disabled; every write feature must fail closed")
	}
	cfg.WebAuth.Enabled = true
	if !cfg.FeatureStorage() {
		t.Fatal("FeatureStorage is false with webAuth enabled and the flag set")
	}
}

func TestCountInheritingStorageProjects(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	// app + api both inherit when no overrides
	if n := cfg.CountInheritingStorageProjects(); n != 2 {
		t.Fatalf("count = %d", n)
	}
	if err := cfg.SetProjectStorageDisabled("app"); err != nil {
		t.Fatal(err)
	}
	// disabled is not nil storage, so not counted as inheriting
	if n := cfg.CountInheritingStorageProjects(); n != 1 {
		t.Fatalf("after disable count = %d", n)
	}
}
