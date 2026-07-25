package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

const deploySecretValue = "postgres://user:hunter2@db/prod"

func TestProjectConfigDeployPageRenders(t *testing.T) {
	srv, cfg, _ := testServer(t)
	timeout := 900_000
	if err := cfg.SetProjectDeployEnabled("proj", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("proj", "prod", config.DeployEnvPolicy{
		RequireCapability: "approve",
		AllowedRefs:       []string{"main"},
		StepTimeoutMaxMs:  &timeout,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("proj", "prod", "DB_URL", deploySecretValue, true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvVar("proj", "prod", "K8S_NAMESPACE", "shop-prod", false); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, srv.Handler(), "/config/projects/proj/deploy")
	for _, want := range []string{
		`id="page-project-config-deploy"`,
		// The tab is reachable from every sibling tab.
		`href="/config/projects/proj/deploy"`,
		">prod<",
		">DB_URL<",
		">K8S_NAMESPACE<",
		"approve",
		// Effective ceiling rendered through msDur.
		"15m",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("deploy settings page missing %q", want)
		}
	}
}

// TestProjectConfigDeployNeverRendersSecret is the load-bearing one: the page
// lists variable names so an admin can manage them, and must never be able to
// echo a stored credential back.
func TestProjectConfigDeployNeverRendersSecret(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectDeployEnvVar("proj", "prod", "DB_URL", deploySecretValue, true); err != nil {
		t.Fatal(err)
	}
	// A non-secret value must not be rendered either: the page carries names only.
	if err := cfg.SetProjectDeployEnvVar("proj", "prod", "K8S_NAMESPACE", "shop-prod", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/config/projects/proj/deploy",
		"/config/projects/proj",
		"/config/projects/proj/integrations",
		"/config",
	} {
		body := getBody(t, srv.Handler(), path)
		for _, leak := range []string{deploySecretValue, "hunter2", "shop-prod"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q", path, leak)
			}
		}
	}
}

func postForm(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestSetProjectDeployEnvVarStoresAndAudits(t *testing.T) {
	srv, cfg, _ := testServer(t)
	w := postForm(t, srv, "/config/projects/deploy/var", url.Values{
		"name":   {"proj"},
		"env":    {"prod"},
		"key":    {"DB_URL"},
		"value":  {deploySecretValue},
		"secret": {"1"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status = %d, want a redirect\n%s", w.Code, w.Body.String())
	}
	env, ok := cfg.ProjectDeployEnv("proj", "prod")
	if !ok || env.Env["DB_URL"] != deploySecretValue || !env.IsSecretKey("DB_URL") {
		t.Fatalf("variable not stored as a secret: %+v", env)
	}
	assertAuditAction(t, srv, "config.set_project_deploy_env_var", true)
	// The audit trail records which credential changed, by name.
	assertAuditDetailContains(t, srv, "DB_URL")
	// It must not become a second place the value lives.
	assertAuditDetailOmits(t, srv, deploySecretValue)
	assertAuditDetailOmits(t, srv, "hunter2")
}

// assertAuditDetailOmits is the inverse of assertAuditDetailContains: it fails
// when a substring appears anywhere in the audit log.
func assertAuditDetailOmits(t *testing.T, srv *Server, substr string) {
	t.Helper()
	if srv.audit == nil {
		t.Fatal("no audit")
	}
	entries, err := os.ReadDir(srv.audit.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(srv.audit.Dir(), ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), substr) {
			t.Fatalf("audit log contains %q", substr)
		}
	}
}

func TestSetProjectDeployEnvRejectsUnknownCapability(t *testing.T) {
	srv, cfg, _ := testServer(t)
	w := postForm(t, srv, "/config/projects/deploy/environment", url.Values{
		"name":              {"proj"},
		"env":               {"prod"},
		"requireCapability": {"wizard"},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("redirect = %q, want an err flash", loc)
	}
	if _, ok := cfg.ProjectDeployEnv("proj", "prod"); ok {
		t.Fatal("environment created despite an invalid capability")
	}
}

func TestSetProjectDeployEnvParsesTimeoutCeiling(t *testing.T) {
	srv, cfg, _ := testServer(t)
	w := postForm(t, srv, "/config/projects/deploy/environment", url.Values{
		"name":           {"proj"},
		"env":            {"stag"},
		"stepTimeoutMax": {"20m"},
		"allowedRefs":    {"main\n# a comment\n\nrelease/*\n"},
	})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("unexpected error redirect: %q", loc)
	}
	env, ok := cfg.ProjectDeployEnv("proj", "stag")
	if !ok {
		t.Fatal("environment missing")
	}
	if env.StepTimeoutMaxMs == nil || *env.StepTimeoutMaxMs != 20*60*1000 {
		t.Fatalf("StepTimeoutMaxMs = %v, want 20m in ms", env.StepTimeoutMaxMs)
	}
	// Comments and blank lines are skipped, like every other textarea list.
	if len(env.AllowedRefs) != 2 || env.AllowedRefs[0] != "main" || env.AllowedRefs[1] != "release/*" {
		t.Fatalf("AllowedRefs = %v", env.AllowedRefs)
	}
}

func TestRemoveProjectDeployEnvVar(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if err := cfg.SetProjectDeployEnvVar("proj", "prod", "DB_URL", "x", true); err != nil {
		t.Fatal(err)
	}
	postForm(t, srv, "/config/projects/deploy/var/remove", url.Values{
		"name": {"proj"}, "env": {"prod"}, "key": {"DB_URL"},
	})
	if env, ok := cfg.ProjectDeployEnv("proj", "prod"); ok {
		if _, still := env.Env["DB_URL"]; still {
			t.Fatal("variable survived removal")
		}
	}
	assertAuditAction(t, srv, "config.remove_project_deploy_env_var", true)
}

func TestSplitLines(t *testing.T) {
	got := splitLines("  main \n\n# comment\nrelease/*\n\t\n")
	if len(got) != 2 || got[0] != "main" || got[1] != "release/*" {
		t.Fatalf("splitLines = %v", got)
	}
	if splitLines("") != nil {
		t.Fatal("empty input should yield nil, not an empty entry")
	}
}
