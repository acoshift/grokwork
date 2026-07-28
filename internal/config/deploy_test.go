package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// deployTestConfig writes a minimal loadable config and returns it loaded.
func deployTestConfig(t *testing.T) (*Config, string) {
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

func reloadConfig(t *testing.T, path string) *Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var again Config
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatalf("reload: %v\n%s", err, raw)
	}
	return &again
}

// TestSaveLockedPreservesDeployRootFields guards the trap that saveLocked
// marshals an explicit whitelist: a root field missing from it is silently
// dropped by the next web config save.
func TestSaveLockedPreservesDeployRootFields(t *testing.T) {
	cfg, path := deployTestConfig(t)
	maxDep, retention := 7, 25
	cfg.MaxConcurrentDeploys = &maxDep
	cfg.DeployRunRetention = &retention

	cfg.mu.Lock()
	err := cfg.saveLocked()
	cfg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	again := reloadConfig(t, path)
	if again.MaxConcurrentDeploys == nil || *again.MaxConcurrentDeploys != 7 {
		t.Fatalf("MaxConcurrentDeploys lost: %+v", again.MaxConcurrentDeploys)
	}
	if again.DeployRunRetention == nil || *again.DeployRunRetention != 25 {
		t.Fatalf("DeployRunRetention lost: %+v", again.DeployRunRetention)
	}
}

// TestProjectWhitelistPreservesDeploy is the same trap one level down: a
// per-project field must appear in ProjectsMap.MarshalJSON's outObj AND in
// cloneProjectsMap, or an unrelated config save deletes every deploy credential.
// No test covered the project whitelist before this one.
func TestProjectWhitelistPreservesDeploy(t *testing.T) {
	cfg, path := deployTestConfig(t)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	timeout := 900_000
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", DeployEnvPolicy{
		RequireCapability: "approve",
		AllowedRefs:       []string{"main"},
		StepTimeoutMaxMs:  &timeout,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "DB_URL", "postgres://secret", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "K8S_NAMESPACE", "shop-prod", false); err != nil {
		t.Fatal(err)
	}

	// An unrelated save must not disturb any of it.
	if err := cfg.SetProjectVerifyCommands("app", []VerifyCommand{{Name: "t", Command: "go test ./..."}}); err != nil {
		t.Fatal(err)
	}

	again := reloadConfig(t, path)
	pc, ok := again.Projects["app"]
	if !ok || pc.Deploy == nil {
		t.Fatalf("deploy config lost entirely: %+v", again.Projects["app"])
	}
	if !pc.Deploy.Enabled {
		t.Fatal("Enabled lost")
	}
	env := pc.Deploy.Environments["prod"]
	if env == nil {
		t.Fatalf("prod environment lost: %+v", pc.Deploy.Environments)
	}
	if env.RequireCapability != "approve" {
		t.Fatalf("RequireCapability = %q", env.RequireCapability)
	}
	if len(env.AllowedRefs) != 1 || env.AllowedRefs[0] != "main" {
		t.Fatalf("AllowedRefs = %v", env.AllowedRefs)
	}
	if env.StepTimeoutMaxMs == nil || *env.StepTimeoutMaxMs != 900_000 {
		t.Fatalf("StepTimeoutMaxMs = %v", env.StepTimeoutMaxMs)
	}
	if env.Env["DB_URL"] != "postgres://secret" || env.Env["K8S_NAMESPACE"] != "shop-prod" {
		t.Fatalf("Env lost: %+v", env.Env)
	}
	if !slices.Contains(env.SecretKeys, "DB_URL") {
		t.Fatalf("SecretKeys lost: %v", env.SecretKeys)
	}
	if slices.Contains(env.SecretKeys, "K8S_NAMESPACE") {
		t.Fatal("non-secret key marked secret")
	}
}

// TestSnapshotNeverExposesDeploySecret is the rule that keeps a credential out
// of every template: Snapshot carries names and a flag, never a value.
func TestSnapshotNeverExposesDeploySecret(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	const secret = "postgres://user:hunter2@db/prod"
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "DB_URL", secret, true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "K8S_NAMESPACE", "shop-prod", false); err != nil {
		t.Fatal(err)
	}

	snap := cfg.Snapshot()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	// The whole snapshot is what templates see; scan all of it, not just the
	// field we expect the value to have been in.
	if strings.Contains(string(blob), "hunter2") || strings.Contains(string(blob), secret) {
		t.Fatalf("Snapshot leaked a deploy secret:\n%s", blob)
	}
	// A non-secret value is equally absent: Snapshot carries no values at all.
	if strings.Contains(string(blob), "shop-prod") {
		t.Fatalf("Snapshot leaked a deploy env value:\n%s", blob)
	}

	var item *ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == "app" {
			item = &snap.Projects[i]
		}
	}
	if item == nil {
		t.Fatal("project missing from snapshot")
	}
	if len(item.DeployEnvs) != 1 || item.DeployEnvs[0].Name != "prod" {
		t.Fatalf("DeployEnvs = %+v", item.DeployEnvs)
	}
	keys := item.DeployEnvs[0].Keys
	if len(keys) != 2 || keys[0].Key != "DB_URL" || !keys[0].Secret || keys[1].Key != "K8S_NAMESPACE" || keys[1].Secret {
		t.Fatalf("Keys = %+v, want DB_URL(secret) and K8S_NAMESPACE(plain) in name order", keys)
	}
	if item.DeployEnvs[0].SecretCount != 1 {
		t.Fatalf("SecretCount = %d, want 1", item.DeployEnvs[0].SecretCount)
	}
}

// TestSetProjectDeployEnvVarKeepsValueWhenBlank pins the "leave blank to keep"
// contract, so re-marking a key as secret does not require retyping a credential
// nobody can read back from the UI.
func TestSetProjectDeployEnvVarKeepsValueWhenBlank(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "TOKEN", "abc123", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "TOKEN", "", true); err != nil {
		t.Fatal(err)
	}
	env, ok := cfg.ProjectDeployEnv("app", "prod")
	if !ok {
		t.Fatal("environment missing")
	}
	if env.Env["TOKEN"] != "abc123" {
		t.Fatalf("value = %q, want the stored one kept", env.Env["TOKEN"])
	}
	if !env.IsSecretKey("TOKEN") {
		t.Fatal("secret flag not applied")
	}
	// A brand new key with no value is a mistake, not an empty variable.
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "FRESH", "", true); err == nil {
		t.Fatal("accepted a new variable with no value")
	}
}

func TestSetProjectDeployEnvVarRejectsReservedNames(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	// The runner injects these itself; letting config set them would let a
	// project forge the identity a step sees.
	for _, key := range []string{"GW_SHA", "GW_ACTOR", "GROK_WORK_CONFIG"} {
		if err := cfg.SetProjectDeployEnvVar("app", "prod", key, "x", false); err == nil {
			t.Fatalf("accepted reserved key %q", key)
		}
	}
	for _, key := range []string{"1BAD", "has-dash", "has space", ""} {
		if err := cfg.SetProjectDeployEnvVar("app", "prod", key, "x", false); err == nil {
			t.Fatalf("accepted malformed key %q", key)
		}
	}
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "KUBECONFIG", "/etc/k.conf", false); err != nil {
		t.Fatalf("rejected a legitimate key: %v", err)
	}
}

func TestSetProjectDeployEnvPolicyValidates(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", DeployEnvPolicy{RequireCapability: "wizard"}); err == nil {
		t.Fatal("accepted an unknown capability")
	}
	over := DefaultDeployStepTimeoutMaxMs + 1
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", DeployEnvPolicy{StepTimeoutMaxMs: &over}); err == nil {
		t.Fatal("accepted a ceiling above the hard maximum")
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", DeployEnvPolicy{StepTimeoutMaxMs: new(0)}); err == nil {
		t.Fatal("accepted a non-positive ceiling")
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "", DeployEnvPolicy{}); err == nil {
		t.Fatal("accepted an empty environment name")
	}
	if err := cfg.SetProjectDeployEnvPolicy("nope", "prod", DeployEnvPolicy{}); err == nil {
		t.Fatal("accepted an unknown project")
	}
}

// TestSetProjectDeployEnvPolicyKeepsSecrets pins that editing policy cannot
// clear a credential by omission — policy and secrets have separate write paths.
func TestSetProjectDeployEnvPolicyKeepsSecrets(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "TOKEN", "abc123", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", DeployEnvPolicy{RequireCapability: "approve"}); err != nil {
		t.Fatal(err)
	}
	env, _ := cfg.ProjectDeployEnv("app", "prod")
	if env == nil || env.Env["TOKEN"] != "abc123" || !env.IsSecretKey("TOKEN") {
		t.Fatalf("policy edit disturbed secrets: %+v", env)
	}
}

func TestRemoveProjectDeployEnvVarClearsSecretMark(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	if err := cfg.SetProjectDeployEnvVar("app", "prod", "TOKEN", "abc", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RemoveProjectDeployEnvVar("app", "prod", "TOKEN"); err != nil {
		t.Fatal(err)
	}
	env, ok := cfg.ProjectDeployEnv("app", "prod")
	if !ok {
		// Removing the last variable may leave an empty policy object; either is
		// fine, but a dangling SecretKeys entry is not.
		return
	}
	if _, still := env.Env["TOKEN"]; still {
		t.Fatal("value survived removal")
	}
	if env.IsSecretKey("TOKEN") {
		t.Fatal("stale SecretKeys entry left behind")
	}
}

// TestNormalizeDropsStaleSecretKeys covers hand-edited config.json: a SecretKeys
// entry naming a variable that no longer exists would otherwise make the UI
// claim a secret is stored when nothing is.
func TestNormalizeDropsStaleSecretKeys(t *testing.T) {
	var m ProjectsMap
	err := json.Unmarshal([]byte(`{"app": {"path": "/tmp/x", "deploy": {
		"enabled": true,
		"environments": {"prod": {"env": {"A": "1"}, "secretKeys": ["A", "GONE"]}}
	}}}`), &m)
	if err != nil {
		t.Fatal(err)
	}
	env := m["app"].Deploy.Environments["prod"]
	if !slices.Contains(env.SecretKeys, "A") {
		t.Fatalf("live secret key dropped: %v", env.SecretKeys)
	}
	if slices.Contains(env.SecretKeys, "GONE") {
		t.Fatalf("stale secret key kept: %v", env.SecretKeys)
	}
}

func TestNormalizeDropsEmptyDeployConfig(t *testing.T) {
	var m ProjectsMap
	// An all-empty deploy block must not persist as {} noise in config.json.
	if err := json.Unmarshal([]byte(`{"app": {"path": "/tmp/x", "deploy": {}}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m["app"].Deploy != nil {
		t.Fatalf("empty deploy config kept: %+v", m["app"].Deploy)
	}
}

func TestDeployDefaults(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	if got := cfg.MaxConcurrentDeploysValue(); got != DefaultMaxConcurrentDeploys {
		t.Fatalf("MaxConcurrentDeploysValue = %d, want %d", got, DefaultMaxConcurrentDeploys)
	}
	if got := cfg.DeployRunRetentionValue(); got != DefaultDeployRunRetention {
		t.Fatalf("DeployRunRetentionValue = %d, want %d", got, DefaultDeployRunRetention)
	}
	var env *DeployEnvConfig
	if got := env.StepTimeoutMaxMsValue(); got != DefaultDeployStepTimeoutMaxMs {
		t.Fatalf("nil env ceiling = %d, want the default", got)
	}
	if env.IsSecretKey("X") {
		t.Fatal("nil env claimed a secret key")
	}
}

func TestDeployEnvSecretValues(t *testing.T) {
	env := &DeployEnvConfig{
		Env:        map[string]string{"A": "secret-a", "B": "plain-b", "C": ""},
		SecretKeys: []string{"A", "C"},
	}
	got := env.SecretValues()
	// Only marked keys with a non-empty value: an empty string would match
	// everywhere in the log and redact the whole stream.
	if len(got) != 1 || got[0] != "secret-a" {
		t.Fatalf("SecretValues = %v, want [secret-a]", got)
	}
}

func TestFeatureDeployFailsClosedWithoutWebAuth(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	cfg.WebAuth = &WebAuthConfig{Enabled: false, Features: WebAuthFeatures{Deploy: true}}
	if cfg.FeatureDeploy() {
		t.Fatal("FeatureDeploy is true with webAuth disabled; every write feature must fail closed")
	}
	cfg.WebAuth.Enabled = true
	if !cfg.FeatureDeploy() {
		t.Fatal("FeatureDeploy is false with webAuth enabled and the flag set")
	}
}

// TestSetProjectDeployEnvRejectsBadNames closes two holes at once: a name the
// lowercase-only manifest can never declare (so the environment would sit dead
// in the UI forever), and a name that becomes an unsafe path segment once lanes
// are stored on disk.
func TestSetProjectDeployEnvRejectsBadNames(t *testing.T) {
	cfg, _ := deployTestConfig(t)
	for _, env := range []string{"Prod", "PROD", "../../etc", "pro/d", "prod ok", "9dev", "-dev", "", strings.Repeat("a", 33)} {
		if err := cfg.SetProjectDeployEnvPolicy("app", env, DeployEnvPolicy{}); err == nil {
			t.Errorf("SetProjectDeployEnvPolicy accepted %q", env)
		}
		if err := cfg.SetProjectDeployEnvVar("app", env, "K", "v", false); err == nil {
			t.Errorf("SetProjectDeployEnvVar accepted %q", env)
		}
	}
	for _, env := range []string{"dev", "prod", "stag", "eu-west-1", "a"} {
		if err := cfg.SetProjectDeployEnvPolicy("app", env, DeployEnvPolicy{}); err != nil {
			t.Errorf("rejected legitimate environment %q: %v", env, err)
		}
	}
}
