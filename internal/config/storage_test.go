package config

import (
	"encoding/json"
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
  "projects": {"app": {"path": "` + dir + `", "allowedUserIds": ["u1"]}},
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

	// Snapshot projection.
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
		{"clear", "", "ignored", "", true},
		{"too-short", "ab", "", "", false},
		{"upper", "Acme-Files", "", "", false},
		{"leading-slash-prefix", "acme-files", "/bad", "", false},
		{"dotdot-prefix", "acme-files", "a/../b", "", false},
		{"wildcard-prefix", "acme-files", "foo*", "", false},
		{"wildcard-bucket", "acme*", "", "", false},
		{"control-prefix", "acme-files", "a\nb", "", false},
		{"relative-creds", "acme-files", "", "keys/svc.json", false},
		{"control-creds", "acme-files", "", "/etc/keys/a\nb.json", false},
		{"unknown-project", "acme-files", "", "", false}, // wrong project name below
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

	// Clear unlinks — including a stored credentials path, which must not
	// survive to silently re-apply on the next link.
	if err := cfg.SetProjectStorageGCS("app", "acme-files", "p", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageGCS("app", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if st := cfg.ProjectStorage("app"); st != nil {
		t.Fatalf("clear left storage: %+v", st)
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

// A relative stored credentials path is a load error for the same reason a bad
// bucket is: it would silently resolve against whatever cwd the bot started in.
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
