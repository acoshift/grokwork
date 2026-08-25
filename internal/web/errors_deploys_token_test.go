package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
)

const mintedDeploysToken = "minted-deploys-secret-do-not-leak"

func TestGenerateDeploysErrorsTokenStoresAndDoesNotLeak(t *testing.T) {
	srv, cfg, _ := testServer(t)
	var gotProject string
	srv.deploysCLI = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "deploys" {
			t.Errorf("bin=%q", name)
		}
		for i, a := range args {
			if a == "-project" && i+1 < len(args) {
				gotProject = args[i+1]
			}
		}
		return []byte(`{"token":"` + mintedDeploysToken + `","expiresAt":"2026-08-25T12:00:00Z","project":"acme"}`), nil
	}

	w := postForm(t, srv, "/config/projects/errors-deploys/generate-token", url.Values{
		"name":       {"proj"},
		"enabled":    {"1"},
		"project":    {"acme"},
		"location":   {"gke.cluster-rcf2"},
		"deployment": {"api"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/config/projects/proj/integrations") || !strings.Contains(loc, "ok=") {
		t.Fatalf("redirect=%q", loc)
	}
	if strings.Contains(loc, mintedDeploysToken) {
		t.Fatalf("redirect leaked token: %q", loc)
	}
	if gotProject != "acme" {
		t.Fatalf("cli project=%q", gotProject)
	}
	if cfg.ProjectDeploysAPIToken("proj") != mintedDeploysToken {
		t.Fatalf("stored=%q", cfg.ProjectDeploysAPIToken("proj"))
	}
	d := cfg.ProjectDeploysErrors("proj")
	if d == nil || !d.Enabled || d.Project != "acme" || d.Location != "gke.cluster-rcf2" || d.Deployment != "api" {
		t.Fatalf("saved block=%+v", d)
	}
	assertAuditAction(t, srv, audit.ActionConfigGenerateErrorsDeploysToken, true)
	assertAuditDetailOmits(t, srv, mintedDeploysToken)

	body := getBody(t, srv.Handler(), "/config/projects/proj/integrations")
	if strings.Contains(body, mintedDeploysToken) {
		t.Fatal("page leaked token")
	}
	if !strings.Contains(body, "credentials set") {
		t.Fatalf("want credentials badge:\n%s", body)
	}
}

func TestGenerateDeploysErrorsTokenUnknownProjectDoesNotMint(t *testing.T) {
	srv, _, _ := testServer(t)
	called := false
	srv.deploysCLI = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return []byte(`{"token":"` + mintedDeploysToken + `"}`), nil
	}
	w := postForm(t, srv, "/config/projects/errors-deploys/generate-token", url.Values{
		"name":    {"nope"},
		"enabled": {"1"},
		"project": {"acme"},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("redirect=%q", loc)
	}
	if called {
		t.Fatal("must not mint when the grokwork project does not exist")
	}
}

func TestGenerateDeploysErrorsTokenRequiresProject(t *testing.T) {
	srv, cfg, _ := testServer(t)
	called := false
	srv.deploysCLI = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	w := postForm(t, srv, "/config/projects/errors-deploys/generate-token", url.Values{
		"name":    {"proj"},
		"enabled": {"1"},
		"project": {"  "},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("redirect=%q", loc)
	}
	if called {
		t.Fatal("must not exec without a deploys.app project")
	}
	if cfg.ProjectDeploysAPIToken("proj") != "" {
		t.Fatal("stored a token on a missing project")
	}
}

func TestGenerateDeploysErrorsTokenCLIErrorDoesNotStore(t *testing.T) {
	srv, cfg, _ := testServer(t)
	srv.deploysCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"token":"` + mintedDeploysToken + `"}`), fmt.Errorf("forbidden")
	}
	w := postForm(t, srv, "/config/projects/errors-deploys/generate-token", url.Values{
		"name":    {"proj"},
		"enabled": {"1"},
		"project": {"acme"},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("redirect=%q", loc)
	}
	if strings.Contains(loc, mintedDeploysToken) {
		t.Fatalf("redirect leaked token: %q", loc)
	}
	if cfg.ProjectDeploysAPIToken("proj") != "" {
		t.Fatal("stored a token after CLI failure")
	}
	assertAuditAction(t, srv, audit.ActionConfigGenerateErrorsDeploysToken, false)
	assertAuditDetailOmits(t, srv, mintedDeploysToken)
}

func TestGenerateDeploysErrorsTokenButtonRenders(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/config/projects/proj/integrations", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="project-errors-deploys"`,
		`formaction="/config/projects/errors-deploys/generate-token"`,
		">Generate token</button>",
		`data-confirm-title="Generate token"`,
		"deploys me generate-token",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestGenerateDeploysErrorsTokenFlashNamesExpiry(t *testing.T) {
	srv, _, _ := testServer(t)
	exp := time.Date(2026, 8, 25, 15, 4, 0, 0, time.UTC)
	srv.deploysCLI = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"token":"` + mintedDeploysToken + `","expiresAt":"` + exp.Format(time.RFC3339) + `"}`), nil
	}
	w := postForm(t, srv, "/config/projects/errors-deploys/generate-token", url.Values{
		"name":    {"proj"},
		"project": {"acme"},
	})
	loc, err := url.QueryUnescape(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loc, "expires 2026-08-25 15:04 UTC") {
		t.Fatalf("flash should name expiry: %q", loc)
	}
}
