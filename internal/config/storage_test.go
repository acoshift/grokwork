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
	if st.Backend != StorageBackendGCS {
		t.Fatalf("backend = %q, want gcs", st.Backend)
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
	if item.StorageBackend != StorageBackendGCS {
		t.Fatalf("StorageBackend = %q", item.StorageBackend)
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
	if st.Backend != StorageBackendGCS {
		t.Fatalf("backend = %q", st.Backend)
	}
	if st.GCSBucket != "acme-company-files" || st.Prefix != "grokwork" || st.CredentialsFile != "/etc/keys/g.json" {
		t.Fatalf("global = %+v", st)
	}
	snap := again.Snapshot()
	if !snap.StorageConfigured || snap.StorageBucket != "acme-company-files" || snap.StoragePrefix != "grokwork" {
		t.Fatalf("snapshot global = configured=%v %q/%q", snap.StorageConfigured, snap.StorageBucket, snap.StoragePrefix)
	}
	if snap.StorageBackend != StorageBackendGCS {
		t.Fatalf("StorageBackend = %q", snap.StorageBackend)
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
	if eff.Backend != StorageBackendGCS {
		t.Fatalf("inherit backend = %q", eff.Backend)
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

// --- Multi-backend / Drive ---

func TestNormalizeOmitBackendBucketIsGCS(t *testing.T) {
	// Migration: omit backend + non-empty gcsBucket → gcs.
	in := &StorageConfig{GCSBucket: "acme-files", Prefix: "gw"}
	got, err := normalizeStorage(in, false)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Backend != StorageBackendGCS || got.GCSBucket != "acme-files" {
		t.Fatalf("got = %+v", got)
	}
	if got.DriveFolderID != "" {
		t.Fatalf("foreign folder id leaked: %q", got.DriveFolderID)
	}
}

func TestNormalizeOmitBackendFolderIsGDrive(t *testing.T) {
	in := &StorageConfig{
		DriveFolderID:   "0ABcdEfghIjKlMnOp",
		CredentialsFile: "/etc/keys/drive.json",
	}
	got, err := normalizeStorage(in, false)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Backend != StorageBackendGDrive || got.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("got = %+v", got)
	}
	if got.GCSBucket != "" || got.Prefix != "" {
		t.Fatalf("foreign gcs fields leaked: %+v", got)
	}
}

func TestNormalizeBothIdentitiesRequireBackend(t *testing.T) {
	in := &StorageConfig{
		GCSBucket:       "acme-files",
		DriveFolderID:   "0ABcdEfghIjKlMnOp",
		CredentialsFile: "/etc/keys/k.json",
	}
	_, err := normalizeStorage(in, false)
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("want backend required error, got %v", err)
	}
}

func TestNormalizeGDriveRequiresCredentials(t *testing.T) {
	in := &StorageConfig{
		Backend:       StorageBackendGDrive,
		DriveFolderID: "0ABcdEfghIjKlMnOp",
	}
	_, err := normalizeStorage(in, false)
	if err == nil || !strings.Contains(err.Error(), "credentialsFile") {
		t.Fatalf("want credentials required, got %v", err)
	}
}

func TestNormalizeProjectGDriveAllowsEmptyCredentials(t *testing.T) {
	in := &StorageConfig{
		Backend:       StorageBackendGDrive,
		DriveFolderID: "0ABcdEfghIjKlMnOp",
	}
	got, err := normalizeStorage(in, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Backend != StorageBackendGDrive || got.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("got = %+v", got)
	}
	if got.CredentialsFile != "" {
		t.Fatalf("empty creds should stay empty on the stored block, got %q", got.CredentialsFile)
	}
}

func TestNormalizeGDriveStripsGCSFields(t *testing.T) {
	in := &StorageConfig{
		Backend:         StorageBackendGDrive,
		DriveFolderID:   "0ABcdEfghIjKlMnOp",
		GCSBucket:       "acme-leftover",
		Prefix:          "leftover",
		CredentialsFile: "/etc/keys/drive.json",
	}
	got, err := normalizeStorage(in, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.GCSBucket != "" || got.Prefix != "" {
		t.Fatalf("gcs fields not stripped: %+v", got)
	}
	if got.Backend != StorageBackendGDrive {
		t.Fatalf("backend = %q", got.Backend)
	}
}

func TestNormalizeGCSStripsDriveFields(t *testing.T) {
	in := &StorageConfig{
		Backend:       StorageBackendGCS,
		GCSBucket:     "acme-files",
		DriveFolderID: "0ABcdEfghIjKlMnOp",
	}
	got, err := normalizeStorage(in, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.DriveFolderID != "" {
		t.Fatalf("drive field not stripped: %+v", got)
	}
}

func TestNormalizeDriveFolderIDFromURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"0ABcdEfghIjKlMnOp", "0ABcdEfghIjKlMnOp"},
		{"https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp", "0ABcdEfghIjKlMnOp"},
		{"https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp?usp=sharing", "0ABcdEfghIjKlMnOp"},
		{"https://drive.google.com/drive/u/0/folders/1AbCdEfGhIjKlMnOpQr", "1AbCdEfGhIjKlMnOpQr"},
		{"folders/0ABcdEfghIjKlMnOp", "0ABcdEfghIjKlMnOp"},
	}
	for _, tc := range cases {
		got, err := normalizeDriveFolderID(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q → %q want %q", tc.in, got, tc.want)
		}
	}
	// bad
	for _, bad := range []string{"", "short", "has space idxx", "../escapeidxx", "id with/slash"} {
		if _, err := normalizeDriveFolderID(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}

func TestStorageHasIdentity(t *testing.T) {
	if storageHasIdentity(nil) {
		t.Fatal("nil")
	}
	if storageHasIdentity(&StorageConfig{Disabled: true}) {
		t.Fatal("disabled")
	}
	if !storageHasIdentity(&StorageConfig{Backend: StorageBackendGCS, GCSBucket: "bkt"}) {
		t.Fatal("gcs")
	}
	if !storageHasIdentity(&StorageConfig{Backend: StorageBackendGDrive, DriveFolderID: "0ABcdEfghIjKlMnOp"}) {
		t.Fatal("gdrive")
	}
	// Defense: empty backend + bucket → true; folder-only → false.
	if !storageHasIdentity(&StorageConfig{GCSBucket: "bkt"}) {
		t.Fatal("legacy empty backend + bucket")
	}
	if storageHasIdentity(&StorageConfig{DriveFolderID: "0ABcdEfghIjKlMnOp"}) {
		t.Fatal("empty backend + folder must fail closed")
	}
	if storageHasIdentity(&StorageConfig{Backend: "other", GCSBucket: "bkt"}) {
		t.Fatal("unknown backend")
	}
}

func TestEffectiveStorageInheritsEmptyProjectCredentials(t *testing.T) {
	cfg, path := storageTestConfig(t)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageDrive("app", "1OverrideFolderID", ""); err != nil {
		t.Fatal(err)
	}
	raw := cfg.ProjectStorage("app")
	if raw == nil || raw.CredentialsFile != "" {
		t.Fatalf("stored override must keep credentials empty: %+v", raw)
	}
	eff := cfg.EffectiveStorage("app")
	if eff == nil {
		t.Fatal("nil effective")
	}
	if eff.DriveFolderID != "1OverrideFolderID" || eff.IsolationSegment != "" {
		t.Fatalf("override identity = %+v", eff)
	}
	if eff.CredentialsFile != "/etc/keys/drive.json" {
		t.Fatalf("effective creds = %q, want global path", eff.CredentialsFile)
	}

	// GCS override with empty creds also picks up the global key.
	if err := cfg.SetProjectStorageGCS("api", "acme-app-private", "prod", ""); err != nil {
		t.Fatal(err)
	}
	gcsEff := cfg.EffectiveStorage("api")
	if gcsEff == nil || gcsEff.GCSBucket != "acme-app-private" || gcsEff.Prefix != "prod" {
		t.Fatalf("gcs override = %+v", gcsEff)
	}
	if gcsEff.CredentialsFile != "/etc/keys/drive.json" {
		t.Fatalf("gcs effective creds = %q, want global path", gcsEff.CredentialsFile)
	}

	// Explicit project key wins.
	if err := cfg.SetProjectStorageDrive("app", "1OverrideFolderID", "/etc/keys/other.json"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveStorage("app"); got == nil || got.CredentialsFile != "/etc/keys/other.json" {
		t.Fatalf("explicit project creds = %+v", got)
	}

	// Empty project + empty global stays empty (GCS ADC / Drive fails at use).
	if err := cfg.SetGlobalStorageGCS("acme-company-files", "gw", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageGCS("app", "acme-app-private", "prod", ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveStorage("app"); got == nil || got.CredentialsFile != "" {
		t.Fatalf("no global key should leave effective empty: %+v", got)
	}

	// Reload: empty project Drive + global Drive key still inherits.
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageDrive("app", "1OverrideFolderID", ""); err != nil {
		t.Fatal(err)
	}
	again := reloadConfig(t, path)
	if got := again.ProjectStorage("app"); got == nil || got.CredentialsFile != "" {
		t.Fatalf("reloaded raw creds = %+v", got)
	}
	if got := again.EffectiveStorage("app"); got == nil || got.CredentialsFile != "/etc/keys/drive.json" {
		t.Fatalf("reloaded effective creds = %+v", got)
	}
}

func TestEffectiveStorageDriveInherit(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	eff := cfg.EffectiveStorage("api")
	if eff == nil {
		t.Fatal("nil effective")
	}
	if eff.Backend != StorageBackendGDrive {
		t.Fatalf("backend = %q", eff.Backend)
	}
	if eff.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("folder = %q", eff.DriveFolderID)
	}
	if eff.IsolationSegment != "api" {
		t.Fatalf("IsolationSegment = %q, want api", eff.IsolationSegment)
	}
	if eff.GCSBucket != "" || eff.Prefix != "" {
		t.Fatalf("gcs fields on drive inherit: %+v", eff)
	}
	// Override uses folder as-is, no isolation.
	if err := cfg.SetProjectStorageDrive("app", "1OverrideFolderID", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	ov := cfg.EffectiveStorage("app")
	if ov == nil || ov.DriveFolderID != "1OverrideFolderID" || ov.IsolationSegment != "" {
		t.Fatalf("override = %+v", ov)
	}
	// Snapshot
	snap := cfg.Snapshot()
	if !snap.StorageConfigured || snap.StorageBackend != StorageBackendGDrive || snap.StorageDriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("global snap = %+v", snap)
	}
	for _, p := range snap.Projects {
		if p.Name == "api" {
			if p.StorageSource != StorageSourceGlobal || p.StorageEffectiveIsolation != "api" {
				t.Fatalf("api snap source=%q isol=%q", p.StorageSource, p.StorageEffectiveIsolation)
			}
			if p.StorageEffectiveDriveFolderID != "0ABcdEfghIjKlMnOp" {
				t.Fatalf("api effective folder = %q", p.StorageEffectiveDriveFolderID)
			}
			if p.StorageDriveFolderID != "" {
				t.Fatalf("raw folder on inherit should be empty, got %q", p.StorageDriveFolderID)
			}
		}
		if p.Name == "app" {
			if p.StorageSource != StorageSourceOverride || p.StorageDriveFolderID != "1OverrideFolderID" {
				t.Fatalf("app snap = %+v", p)
			}
			if p.StorageEffectiveIsolation != "" {
				t.Fatalf("override isolation should be empty, got %q", p.StorageEffectiveIsolation)
			}
		}
	}
}

func TestClearVsDisableUnderGlobalDrive(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageDrive("app", "1OverrideFolderID", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ClearProjectStorage("app"); err != nil {
		t.Fatal(err)
	}
	eff := cfg.EffectiveStorage("app")
	if eff == nil || eff.IsolationSegment != "app" || eff.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("clear under global drive: %+v", eff)
	}
	if err := cfg.SetProjectStorageDisabled("app"); err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveStorage("app") != nil {
		t.Fatal("disable under global drive must nil effective")
	}
	// Global clear via empty input — identity-based, not bucket-empty.
	if err := cfg.SetGlobalStorage(StorageInput{}); err != nil {
		t.Fatal(err)
	}
	if cfg.GlobalStorage() != nil {
		t.Fatal("global clear left storage")
	}
	if cfg.Snapshot().StorageConfigured {
		t.Fatal("StorageConfigured after clear")
	}
	// Disable alone still leaves raw disabled; clear re-inherits nothing.
	if err := cfg.ClearProjectStorage("app"); err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveStorage("app") != nil {
		t.Fatal("clear without global must nil")
	}
}

func TestSetGlobalStorageDriveRoundTrip(t *testing.T) {
	cfg, path := storageTestConfig(t)
	if err := cfg.SetGlobalStorageDrive(
		"https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp",
		"/etc/keys/drive.json",
	); err != nil {
		t.Fatal(err)
	}
	again := reloadConfig(t, path)
	st := again.GlobalStorage()
	if st == nil || st.Backend != StorageBackendGDrive || st.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("reloaded = %+v", st)
	}
	// Persisted JSON has backend and no gcsBucket.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"backend": "gdrive"`) && !strings.Contains(string(raw), `"backend":"gdrive"`) {
		t.Fatalf("backend not persisted: %s", raw)
	}
	if strings.Contains(string(raw), "gcsBucket") {
		t.Fatalf("gcsBucket should be stripped: %s", raw)
	}
}

func TestSetProjectStorageGCSStillWorks(t *testing.T) {
	// Wrapper must keep working after multi-backend rewrite.
	cfg, path := storageTestConfig(t)
	if err := cfg.SetProjectStorageGCS("app", "acme-app", "p", ""); err != nil {
		t.Fatal(err)
	}
	again := reloadConfig(t, path)
	st := again.ProjectStorage("app")
	if st == nil || st.Backend != StorageBackendGCS || st.GCSBucket != "acme-app" {
		t.Fatalf("got = %+v", st)
	}
}

func TestSetProjectStorageEmptyIdentityErrors(t *testing.T) {
	cfg, _ := storageTestConfig(t)
	err := cfg.SetProjectStorage("app", StorageInput{})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "ClearProjectStorage") {
		t.Fatalf("error should name ClearProjectStorage: %v", err)
	}
}

func TestUnknownBackendErrors(t *testing.T) {
	_, err := normalizeStorage(&StorageConfig{
		Backend:   "s3",
		GCSBucket: "acme-files",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("got %v", err)
	}
}

func TestIsolationSegmentNeverPersisted(t *testing.T) {
	cfg, path := storageTestConfig(t)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	// Stamp isolation on effective only.
	eff := cfg.EffectiveStorage("app")
	if eff == nil || eff.IsolationSegment == "" {
		t.Fatalf("want isolation on effective: %+v", eff)
	}
	// Force-write a hand-built block with IsolationSegment set in memory and save.
	cfg.mu.Lock()
	cfg.Storage.IsolationSegment = "should-not-persist"
	if err := cfg.saveLocked(); err != nil {
		cfg.mu.Unlock()
		t.Fatal(err)
	}
	cfg.mu.Unlock()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "should-not-persist") {
		t.Fatalf("IsolationSegment value leaked to disk: %s", raw)
	}
	if strings.Contains(string(raw), `"isolationSegment"`) {
		t.Fatalf("isolationSegment key leaked to disk: %s", raw)
	}
	// Normalize clears it.
	got, err := normalizeStorage(&StorageConfig{
		Backend:          StorageBackendGDrive,
		DriveFolderID:    "0ABcdEfghIjKlMnOp",
		CredentialsFile:  "/etc/keys/drive.json",
		IsolationSegment: "x",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsolationSegment != "" {
		t.Fatalf("normalize left IsolationSegment = %q", got.IsolationSegment)
	}
}
