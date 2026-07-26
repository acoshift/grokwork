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

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
)

// generateServer is the fix fixture (auth on, startSessions on, fake CLIs) plus
// a git repo and a manifest stub, so the deploys page renders for real.
//
// Deliberately does NOT enable the deploy feature: writing the pipeline has to
// be possible before deploys are switched on, and that is the gate this file
// exists to pin.
func generateServer(t *testing.T, manifest string) (*Server, *config.Config) {
	t.Helper()
	srv, cfg, b := fixEnabledServer(t)
	// Every generate/dispatch test in this file posts a form that kicks off an
	// async session and returns immediately (redirect only, run streams in the
	// background). Without this, a test whose assertions finish before the run
	// does can have t.TempDir() cleanup delete the run's cwd/session-store
	// directories out from under a goroutine that is still writing to them.
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	projPath, ok := cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path missing")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	prev := srv.ghRunner
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "git" && strings.HasPrefix(joined, "ls-tree") {
			if manifest == "" {
				// ls-tree exits 0 with no output for a path that does not exist.
				return nil, nil
			}
			return fmt.Appendf(nil, "100644 blob deadbeef  %d\t.grokwork/deploy.yaml", len(manifest)), nil
		}
		if name == "git" && strings.HasPrefix(joined, "cat-file blob") {
			return []byte(manifest), nil
		}
		if prev != nil {
			return prev(ctx, dir, name, args...)
		}
		return nil, nil
	}
	return srv, cfg
}

func getAuthed(t *testing.T, srv *Server, path, sid string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func adminLogin(t *testing.T, srv *Server) (sid, csrf string) {
	t.Helper()
	sid, csrf, err := srv.LoginAs("allow-user", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return sid, csrf
}

// TestGenerateFormNeedsStartSessionsNotDeploy pins the gate choice: writing the
// pipeline is ordinary agent work ending in a PR, so it is gated on session
// starts and must work while deploys themselves are still off.
func TestGenerateFormNeedsStartSessionsNotDeploy(t *testing.T) {
	srv, cfg := generateServer(t, "")
	sid, csrf := adminLogin(t, srv)
	if cfg.FeatureDeploy() {
		t.Fatal("fixture unexpectedly enabled the deploy feature")
	}
	body := getAuthed(t, srv, "/projects/proj/deploys", sid)
	if !strings.Contains(body, `id="deploy-generate"`) {
		t.Fatal("generate form missing with startSessions on and deploy off")
	}
	w := postFix(t, srv, "/projects/proj/deploys/generate", sid, csrf, url.Values{
		"requirements": {"keep it simple"},
	})
	if w.Code == http.StatusNotFound {
		t.Fatal("generate route 404s despite startSessions being on")
	}
}

func TestGenerateFormOffWithoutStartSessions(t *testing.T) {
	srv, cfg := generateServer(t, "")
	cfg.WebAuth.Features.StartSessions = false
	sid, csrf := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/deploys", sid)
	if strings.Contains(body, `id="deploy-generate"`) {
		t.Fatal("generate form rendered with startSessions off")
	}
	w := postFix(t, srv, "/projects/proj/deploys/generate", sid, csrf, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with startSessions off", w.Code)
	}
}

// TestGenerateFormRendersOnBothStates: the form matters most when there is no
// manifest, and stays available afterwards because services change.
func TestGenerateFormRendersOnBothStates(t *testing.T) {
	empty, _ := generateServer(t, "")
	sid, _ := adminLogin(t, empty)
	body := getAuthed(t, empty, "/projects/proj/deploys", sid)
	for _, want := range []string{
		`id="deploy-generate"`,
		`name="requirements"`,
		"Generate pipeline",
		`action="/projects/proj/deploys/generate"`,
		"No pipeline yet",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty-state generate form missing %q", want)
		}
	}

	configured, _ := generateServer(t, testDeployManifest)
	sid2, _ := adminLogin(t, configured)
	body = getAuthed(t, configured, "/projects/proj/deploys", sid2)
	if !strings.Contains(body, `id="deploy-generate"`) {
		t.Fatal("configured board is missing the generate form")
	}
	// Wording flips: editing a working pipeline is a different act from writing one.
	if !strings.Contains(body, "Start update session") {
		t.Fatal("configured board does not offer an update")
	}
}

// TestGenerateFormRendersExactlyOnceInEveryState covers the branch a reader
// forgets: a manifest that fails to parse sets neither DeployNotConfigured nor
// DeployRows, so a form placed inside those branches disappears exactly when a
// broken pipeline most needs an agent. Also pins "exactly once", since placing
// it in both branches would double it.
func TestGenerateFormRendersExactlyOnceInEveryState(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"not configured", ""},
		{"valid manifest", testDeployManifest},
		{"broken manifest", "version: 1\nenvironments: [dev]\nservices:\n  a: {dir: ., stpes: []}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := generateServer(t, tc.manifest)
			sid, _ := adminLogin(t, srv)
			body := getAuthed(t, srv, "/projects/proj/deploys", sid)
			if n := strings.Count(body, `id="deploy-generate"`); n != 1 {
				t.Fatalf("generate form rendered %d times, want exactly 1", n)
			}
		})
	}
}

func TestPostGenerateStartsASession(t *testing.T) {
	srv, _ := generateServer(t, "")
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/projects/proj/deploys/generate", sid, csrf, url.Values{
		"requirements": {"prod must promote the stag image"},
	})
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("generate failed: %s", loc)
	}
	// Lands on the session page, where the run streams.
	if !strings.Contains(loc, "/sessions/") {
		t.Fatalf("redirect = %q, want the session page", loc)
	}
	assertAuditDetailContains(t, srv, "deploy.manifest")
}

func TestPostGenerateRequiresCapability(t *testing.T) {
	srv, cfg := generateServer(t, "")
	// Safe-team mode with an investigator default strips builder-class caps.
	if err := cfg.SetProjectSafeTeamPolicy("proj", true, "investigator"); err != nil {
		t.Fatal(err)
	}
	sid, csrf := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/deploys", sid)
	if strings.Contains(body, `id="deploy-generate"`) {
		t.Fatal("generate form shown to an actor without builder-class caps")
	}
	// Enforced in the handler, not only by hiding the form.
	w := postFix(t, srv, "/projects/proj/deploys/generate", sid, csrf, url.Values{})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("redirect = %q (status %d), want a permission error", loc, w.Code)
	}
}

func TestPostGenerateUnknownProject(t *testing.T) {
	srv, _ := generateServer(t, "")
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/projects/nope/deploys/generate", sid, csrf, url.Values{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// deployFeatureServer enables the deploy feature so the board renders trigger
// forms, with prod gated and dev not.
func deployFeatureServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	srv, cfg := generateServer(t, testDeployManifest)
	cfg.WebAuth.Features.Deploy = true
	if err := cfg.SetProjectDeployEnabled("proj", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("proj", "prod", config.DeployEnvPolicy{
		RequireCapability: "approve", AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("proj", "dev", config.DeployEnvPolicy{
		AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	return srv, cfg
}

// TestGatedCellPostsTheReviewedCommit pins the half the engine cannot enforce:
// the guard refuses a gated trigger with no expectation, so a form that omits
// expect_sha would make every protected deploy fail. It also pins that a gated
// cell does not offer a free-text ref — the commit shown is the commit shipped.
func TestGatedCellPostsTheReviewedCommit(t *testing.T) {
	srv, _ := deployFeatureServer(t)
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/deploys", sid)

	i := strings.Index(body, `name="env" value="prod"`)
	if i < 0 {
		t.Fatalf("no prod trigger form on the board:\n%s", body)
	}
	start := strings.LastIndex(body[:i], "<form")
	end := i + strings.Index(body[i:], "</form>")
	form := body[start:end]

	if !strings.Contains(form, `name="expect_sha"`) {
		t.Fatalf("gated form omits expect_sha, so every prod deploy would be refused:\n%s", form)
	}
	if strings.Contains(form, `type="text" name="ref"`) {
		t.Fatalf("gated form offers a free-text ref; the reviewed commit must be the one shipped:\n%s", form)
	}
	if !strings.Contains(form, `type="hidden" name="ref"`) {
		t.Fatalf("gated form lost its ref:\n%s", form)
	}

	// dev is ungated: the ref stays editable and no expectation is imposed.
	j := strings.Index(body, `name="env" value="dev"`)
	if j < 0 {
		t.Fatal("no dev trigger form")
	}
	dstart := strings.LastIndex(body[:j], "<form")
	dend := j + strings.Index(body[j:], "</form>")
	devForm := body[dstart:dend]
	if !strings.Contains(devForm, `type="text" name="ref"`) {
		t.Fatalf("ungated form lost its editable ref:\n%s", devForm)
	}
	if strings.Contains(devForm, `name="expect_sha"`) {
		t.Fatalf("ungated form imposes a drift check; deploying the current tip is the intent:\n%s", devForm)
	}
}
