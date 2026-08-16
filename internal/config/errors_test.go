package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectErrorsAccessorsAndEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects: ProjectsMap{
			"app":   {Path: filepath.Join(dir, "app")},
			"plain": {Path: filepath.Join(dir, "p")},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}

	if cfg.ProjectSentryEnabled("app") || cfg.ProjectDeploysErrorsEnabled("app") || cfg.ProjectGCPErrorsEnabled("app") {
		t.Fatal("expected off")
	}
	if cfg.ProjectErrorsAnyEnabled("plain") {
		t.Fatal("plain")
	}

	t.Setenv("SENTRY_AUTH_TOKEN_APP", "from-env")
	t.Setenv("DEPLOYS_API_TOKEN_APP", "dep-env")
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "web", "", "", false); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectSentryEnabled("app") {
		t.Fatal("sentry enabled")
	}
	if got := cfg.ProjectSentryAuthToken("app"); got != "from-env" {
		t.Fatalf("env token=%q", got)
	}
	if !cfg.ProjectSentryCanResolve("app") {
		t.Fatal("can resolve with env")
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "web", "from-config", "", false); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectSentryAuthToken("app"); got != "from-config" {
		t.Fatalf("config wins: %q", got)
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "api", "", "", false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectSentry("app").Project != "api" {
		t.Fatal("empty token must leave stored")
	}
	if cfg.ProjectSentryAuthToken("app") != "from-config" {
		t.Fatal("token cleared unexpectedly")
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "api", "", "", true); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectSentryAuthToken("app") != "from-env" {
		t.Fatal("cleared should fall back to env")
	}

	if err := cfg.SetProjectErrorsDeploys("app", true, "acme", "gke.x", "api", "", false); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectDeploysAPIToken("app"); got != "dep-env" {
		t.Fatalf("deploys env=%q", got)
	}
	if !cfg.ProjectDeploysErrorsCanResolve("app") {
		t.Fatal("deploys resolve")
	}
	if ProjectEnvKeySuffix("app") != "APP" {
		t.Fatal(ProjectEnvKeySuffix("app"))
	}
	// Unsuffixed CLI names must not be read.
	t.Setenv("DEPLOYS_TOKEN", "cli-unsuffixed")
	t.Setenv("SENTRY_AUTH_TOKEN", "unsuffixed")
	if cfg.ProjectDeploysAPIToken("plain") == "cli-unsuffixed" {
		t.Fatal("must not read DEPLOYS_TOKEN")
	}
}

func TestProjectDeploysBasicAuthEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectErrorsDeploys("app", true, "acme", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectDeploysErrorsCanResolve("app") {
		t.Fatal("no token no basic")
	}
	t.Setenv("DEPLOYS_AUTH_USER_APP", "sa")
	t.Setenv("DEPLOYS_AUTH_PASS_APP", "secret")
	u, p := cfg.ProjectDeploysBasicAuth("app")
	if u != "sa" || p != "secret" {
		t.Fatalf("%q %q", u, p)
	}
	if !cfg.ProjectDeploysErrorsCanResolve("app") {
		t.Fatal("basic should resolve")
	}
}

func TestSetProjectErrorsGCPRelativeCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := cfg.SetProjectErrorsGCP("app", true, "acme-prod", "", "relative/key.json", "")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want absolute-path error, got %v", err)
	}
	abs := filepath.Join(dir, "missing-key.json")
	if err := cfg.SetProjectErrorsGCP("app", true, "acme-prod", "", abs, "api"); err != nil {
		t.Fatal(err)
	}
	g := cfg.ProjectGCPErrors("app")
	if g == nil || g.CredentialsFile != abs || g.ProjectID != "acme-prod" {
		t.Fatalf("%+v", g)
	}
	if !cfg.ProjectGCPErrorsCanResolve("app") {
		t.Fatal("projectId is enough")
	}
}

func TestSentryBaseURLReject(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Projects:   ProjectsMap{"app": {Path: filepath.Join(dir, "app")}},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "web", "", "http://sentry.example", false); err == nil {
		t.Fatal("http")
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "web", "", "https://user:pass@sentry.example", false); err == nil {
		t.Fatal("userinfo")
	}
	if err := cfg.SetProjectErrorsSentry("app", true, "acme", "web", "", "https://sentry.example:9000", false); err != nil {
		t.Fatal(err)
	}
}

func TestProjectErrorsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "sa.json")
	m := ProjectsMap{
		"app": {
			Path: filepath.Join(dir, "app"),
			Errors: &ProjectErrorsConfig{
				GCP: &ProjectGCPErrorsConfig{
					Enabled: true, ProjectID: "p1", CredentialsFile: abs,
				},
				Sentry: &ProjectSentryConfig{
					Enabled: true, Org: "o", Project: "web", AuthToken: "tok",
				},
				Deploys: &ProjectDeploysErrorsConfig{
					Enabled: true, Project: "acme", Location: "loc", Deployment: "api", APIToken: "dt",
				},
			},
		},
	}
	raw, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"errors"`) {
		t.Fatalf("outObj dropped errors: %s", raw)
	}
	var got ProjectsMap
	if err := got.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	e := got["app"].Errors
	if e == nil || e.GCP == nil || e.GCP.ProjectID != "p1" || e.Sentry.AuthToken != "tok" || e.Deploys.APIToken != "dt" {
		t.Fatalf("%+v", e)
	}
	cloned := cloneProjectsMap(m)
	if cloned["app"].Errors == m["app"].Errors {
		t.Fatal("clone must detach pointer")
	}
	if cloned["app"].Errors.Sentry.AuthToken != "tok" {
		t.Fatal("clone lost token")
	}
}

func TestProjectErrorsNormalizeEmptyNil(t *testing.T) {
	got, err := normalizeProjectErrors(&ProjectErrorsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("empty should nil: %+v", got)
	}
}

func TestProjectErrorsLoadRejectsRelativeCredentials(t *testing.T) {
	raw := []byte(`{"projects":{"app":{"path":"/tmp/app","errors":{"gcp":{"enabled":true,"projectId":"p","credentialsFile":"rel.json"}}}}}`)
	var m ProjectsMap
	if err := m.UnmarshalJSON(raw); err == nil {
		t.Fatal("expected load error")
	}
}

func TestProjectErrorsSnapshotNeverExposesTokens(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "sa.json")
	cfg := &Config{
		Projects: ProjectsMap{
			"app": {
				Path: filepath.Join(dir, "app"),
				Errors: &ProjectErrorsConfig{
					Sentry:  &ProjectSentryConfig{Enabled: true, Org: "o", Project: "web", AuthToken: "secret-token"},
					Deploys: &ProjectDeploysErrorsConfig{Enabled: true, Project: "acme", APIToken: "secret-dep"},
					GCP:     &ProjectGCPErrorsConfig{Enabled: true, ProjectID: "p", CredentialsFile: abs},
				},
			},
		},
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := cfg.Snapshot()
	if len(snap.Projects) != 1 {
		t.Fatalf("projects=%d", len(snap.Projects))
	}
	it := snap.Projects[0]
	if !it.SentryAuthTokenSet || it.SentryOrg != "o" {
		t.Fatalf("%+v", it)
	}
	if it.GCPCredentialsFile != abs {
		t.Fatalf("path=%q", it.GCPCredentialsFile)
	}
	if strings.Contains(it.SentryEnvHint, "secret") || strings.Contains(it.DeploysEnvHint, "secret") {
		t.Fatal("hint leaked")
	}
	if !strings.HasPrefix(it.SentryEnvHint, "SENTRY_AUTH_TOKEN_") {
		t.Fatal(it.SentryEnvHint)
	}
	if !strings.Contains(it.DeploysEnvHint, "DEPLOYS_API_TOKEN_") || strings.Contains(it.DeploysEnvHint, "DEPLOYS_TOKEN") && !strings.Contains(it.DeploysEnvHint, "not DEPLOYS_TOKEN") {
		// hint must name the suffixed var and say it is not DEPLOYS_TOKEN
		if !strings.Contains(it.DeploysEnvHint, "DEPLOYS_API_TOKEN_") {
			t.Fatal(it.DeploysEnvHint)
		}
	}
}
