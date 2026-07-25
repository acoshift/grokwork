package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/deploy"
)

const testDeployManifest = `version: 1
environments: [dev, stag, prod]

x-anchors:
  build: &s_build { name: build, run: "docker build -t $IMAGE_REPO/api:$GW_SHA .", timeout: 20m }

services:
  api:
    dir: services/api
    steps:
      - *s_build
      - { name: apply, run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
    pipelines:
      prod:
        - { name: verify-image, run: "docker manifest inspect $IMAGE_REPO/api:$GW_SHA" }
        - { name: backup-db,    run: "./scripts/backup.sh", timeout: 30m }
        - { name: rollout,      run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
  web:
    dir: apps/web
    envs: [dev, prod]
    steps:
      - { name: build,      run: "npm ci && npm run build" }
      - { name: invalidate, run: "gcloud compute url-maps invalidate-cdn-cache m --path '/*'", envs: [prod] }
`

// deploysServer is a single-repo project with a real git root, and a git stub
// that serves one manifest blob. Deliberately not workflowServer: that fixture
// is pinned byte-for-byte by many tests and has a multi-repo catalog.
func deploysServer(t *testing.T, manifest string) *Server {
	t.Helper()
	srv, cfg, _ := testServer(t)
	projPath, ok := cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path missing")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	srv.ghRunner = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "git" && strings.HasPrefix(joined, "ls-tree"):
			if manifest == "" {
				// ls-tree exits 0 with no output for a path that does not exist.
				return nil, nil
			}
			return []byte(fmt.Sprintf("100644 blob deadbeef  %d\t%s", len(manifest), deploy.DefaultManifestPath)), nil
		case name == "git" && strings.HasPrefix(joined, "cat-file blob"):
			return []byte(manifest), nil
		}
		return nil, fmt.Errorf("unexpected command: %s %s", name, joined)
	}
	return srv
}

// getStatusBody is getBody's sibling for cases that assert on a non-200.
func getStatusBody(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Body.String()
}

func TestDeploysPageRendersManifest(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	for _, want := range []string{
		`id="page-deploys"`,
		// Environment columns come from the manifest, in declared order.
		">dev<", ">stag<", ">prod<",
		// Both services and their dirs.
		">api<", ">web<", ">services/api<", ">apps/web<",
		// Per-environment step counts: api/dev is the 2-step default pipeline,
		// api/prod is the 3-step override, web/prod adds the filtered step.
		"api → dev", "api → prod", "web → prod",
		// A real command reaches the page.
		"docker manifest inspect",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("deploys page missing %q", want)
		}
	}
	// web is narrowed to dev+prod, so its stag cell must not offer a pipeline.
	if strings.Contains(body, "web → stag") {
		t.Fatal("web/stag rendered a pipeline despite the envs narrowing")
	}
	assertNavActive(t, body, "Deploys")
}

func TestDeploysPageNotConfigured(t *testing.T) {
	srv := deploysServer(t, "")
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	for _, want := range []string{
		`id="page-deploys"`,
		"No pipeline yet",
		deploy.DefaultManifestPath,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty state missing %q", want)
		}
	}
	// A missing manifest is the normal state for most projects, not a failure.
	if strings.Contains(body, `class="flash err"`) {
		t.Fatal("missing manifest rendered as an error")
	}
}

func TestDeploysPageShowsManifestError(t *testing.T) {
	srv := deploysServer(t, "version: 1\nenvironments: [dev]\nservices:\n  api: {dir: ., stpes: []}\n")
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	if !strings.Contains(body, `class="flash err"`) {
		t.Fatal("broken manifest did not render an error flash")
	}
	// The strict-decoding message names the typo and its line, which is the whole
	// reason KnownFields is on.
	if !strings.Contains(body, "stpes") {
		t.Fatalf("error does not name the offending key:\n%s", body)
	}
}

func TestDeploysPageForbiddenForUnknownProject(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	code, _ := getStatusBody(t, srv, "/projects/nope/deploys")
	// ensureProjectAccess reports an unknown project the same way as a denied
	// one, and forbiddenProject always answers 403 — never 404.
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

// TestDeploysPageNonGitProjectReportsInline covers a project whose path is not a
// checkout: the page still renders its own chrome and says why, rather than
// 500ing or rendering an empty board that looks like "no services".
func TestDeploysPageNonGitProjectReportsInline(t *testing.T) {
	srv, _, _ := testServer(t) // proj exists but was never git init'd
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	if !strings.Contains(body, `id="page-deploys"`) {
		t.Fatal("page chrome missing")
	}
	if !strings.Contains(body, `class="flash err"`) {
		t.Fatalf("non-git project did not report a reason:\n%s", body)
	}
	if strings.Contains(body, "No pipeline yet") {
		t.Fatal("a non-git project must not read as 'no pipeline configured'")
	}
}

// TestPickedRepo pins the contract that lets the repo picker work with no
// JavaScript. The commits browser needs a JS sync step for the same control, so
// this is the one place the difference can regress unnoticed.
func TestPickedRepo(t *testing.T) {
	cases := []struct {
		name              string
		full, owner, repo string
		wantOwner         string
		wantRepo          string
	}{
		{"picker only", "acme/api", "", "", "acme", "api"},
		{"direct link only", "", "acme", "api", "acme", "api"},
		// The picker is the newer signal, so it wins over stale link params.
		{"picker wins", "acme/web", "acme", "api", "acme", "web"},
		// A combined value with no slash falls back rather than yielding half a pair.
		{"malformed falls back", "nope", "acme", "api", "acme", "api"},
		{"nothing", "", "", "", "", ""},
		{"whitespace trimmed", " acme / api ", "", "", "acme", "api"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := pickedRepo(tc.full, tc.owner, tc.repo)
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("pickedRepo(%q, %q, %q) = (%q, %q), want (%q, %q)",
					tc.full, tc.owner, tc.repo, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestDeploysPageHasNoPickerScript pins that the page does not grow a copy of
// the commits browser's select-mirroring script: the handler parses repo_full
// itself, and a second copy of that contract is what would drift.
func TestDeploysPageHasNoPickerScript(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	if strings.Contains(body, "deploys-owner") || strings.Contains(body, "deploys-repo-pick") {
		t.Fatal("deploys page rendered the hidden-input picker plumbing")
	}
}
