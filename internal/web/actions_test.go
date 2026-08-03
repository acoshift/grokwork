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
	"github.com/acoshift/grokwork/internal/config"
)

const testDispatchWorkflowYAML = `name: Deploy
on:
  workflow_dispatch:
    inputs:
      env:
        type: choice
        options: [dev, prod]
        default: dev
`

const testPushOnlyWorkflowYAML = `name: CI
on:
  push:
jobs:
  t:
    runs-on: ubuntu-latest
    steps: [{run: echo hi}]
`

type actionsGHCall struct {
	name string
	args []string
}

// actionsServer is a githubWrites-enabled fixture with a single-repo catalog,
// git-init'd project path, and a fake gh/git runner that serves workflows +
// optionally records dispatch argv.
func actionsServer(t *testing.T) (*Server, *config.Config, *[]actionsGHCall) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.GitHubWrites = true
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	projPath, ok := cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path missing")
	}
	if err := execGitInit(t, projPath); err != nil {
		t.Fatal(err)
	}
	calls := make([]actionsGHCall, 0, 8)
	srv.ghRunner = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, actionsGHCall{name: name, args: append([]string(nil), args...)})
		joined := name + " " + strings.Join(args, " ")
		switch {
		case name == "gh" && strings.HasPrefix(strings.Join(args, " "), "workflow list"):
			return []byte(`[
				{"id":11,"name":"Deploy","path":".github/workflows/deploy.yml","state":"active"},
				{"id":12,"name":"CI","path":".github/workflows/ci.yml","state":"active"}
			]`), nil
		case name == "gh" && strings.HasPrefix(strings.Join(args, " "), "run list"):
			return []byte(`[
				{"databaseId":99,"number":3,"attempt":1,"displayTitle":"Deploy run",
				 "workflowName":"Deploy","workflowDatabaseId":11,"headBranch":"main",
				 "headSha":"abcdef0123456789","event":"workflow_dispatch","status":"completed",
				 "conclusion":"success","url":"https://github.com/acme/app/actions/runs/99",
				 "createdAt":"2026-08-01T12:00:00Z","updatedAt":"2026-08-01T12:05:00Z"}
			]`), nil
		case name == "gh" && strings.Contains(joined, "workflow run"):
			return nil, nil
		case name == "git" && strings.Contains(joined, "rev-parse --verify"):
			return []byte("abc123deadbeef\n"), nil
		case name == "git" && strings.Contains(joined, "cat-file blob"):
			if strings.Contains(joined, "deploy.yml") {
				return []byte(testDispatchWorkflowYAML), nil
			}
			if strings.Contains(joined, "ci.yml") {
				return []byte(testPushOnlyWorkflowYAML), nil
			}
			return nil, fmt.Errorf("unknown blob: %s", joined)
		case name == "git" && strings.Contains(joined, "for-each-ref"):
			return []byte("main\nfeature\nHEAD\nproduction\n"), nil
		case name == "gh" && strings.Contains(joined, "run view") && strings.Contains(joined, "--log"):
			// Long unwrapped line exercises the log-block overflow constraint.
			return []byte("build\tCheckout\t" + strings.Repeat("x", 400) + "\nbuild\tCheckout\tok\n"), nil
		case name == "gh" && strings.Contains(joined, "run view"):
			return []byte(`{
				"attempt":1,"displayTitle":"Deploy run","workflowName":"Deploy",
				"headBranch":"main","headSha":"abcdef0","event":"workflow_dispatch",
				"status":"completed","conclusion":"success",
				"url":"https://github.com/acme/app/actions/runs/99",
				"createdAt":"2026-08-01T12:00:00Z","updatedAt":"2026-08-01T12:05:00Z",
				"jobs":[{"databaseId":1,"name":"build","status":"completed","conclusion":"success",
				  "url":"https://github.com/acme/app/actions/runs/99/job/1",
				  "startedAt":"2026-08-01T12:00:00Z","completedAt":"2026-08-01T12:01:00Z",
				  "steps":[{"name":"Checkout","status":"completed","conclusion":"success","number":1}]}]
			}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	return srv, cfg, &calls
}

func actionsDispatchCall(calls []actionsGHCall) (actionsGHCall, bool) {
	for _, c := range calls {
		if c.name == "gh" && len(c.args) > 0 && c.args[0] == "workflow" && len(c.args) > 1 && c.args[1] == "run" {
			return c, true
		}
	}
	return actionsGHCall{}, false
}

func assertActionsDispatchAudit(t *testing.T, srv *Server, wantOK bool) {
	t.Helper()
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Action == audit.ActionActionsDispatch && ev.OK == wantOK {
			if !wantOK && strings.TrimSpace(ev.Error) == "" {
				t.Fatalf("refusal must record why: %+v", ev)
			}
			return
		}
	}
	t.Fatalf("no %s audit event (ok=%v): %+v", audit.ActionActionsDispatch, wantOK, evs)
}

func TestActionsPageRenders(t *testing.T) {
	srv, _, _ := actionsServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/actions", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-actions"`) {
		t.Fatal("missing page-actions marker")
	}
	assertNavActive(t, body, "Actions")
	// The shell paints instantly: both sections are async fragments loading on
	// hx-trigger="load" over skeletons — no gh call may block the page render.
	for _, want := range []string{
		"Workflows", "Recent runs",
		`hx-get="/partials/projects/proj/actions/workflows?`,
		`hx-get="/partials/projects/proj/actions/runs?`,
		`hx-trigger="load"`,
		`hx-trigger="load, every 15s"`,
		`class="skel-rows"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("actions page missing %q", want)
		}
	}
	for _, banned := range []string{"Run workflow", "Deploy run"} {
		if strings.Contains(body, banned) {
			t.Fatalf("actions page shell must not render fragment content %q inline", banned)
		}
	}
}

// getActionsFragment fetches a partial with the fixture session.
func getActionsFragment(t *testing.T, srv *Server, sid, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestActionsWorkflowsPartial(t *testing.T) {
	srv, _, _ := actionsServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getActionsFragment(t, srv, sid, "/partials/projects/proj/actions/workflows?owner=acme&repo=app")
	for _, want := range []string{`class="wf-register"`, "Deploy", "Run workflow", `class="wf-tag run"`, ".github/workflows/deploy.yml"} {
		if !strings.Contains(body, want) {
			t.Fatalf("workflows partial missing %q:\n%s", want, body)
		}
	}
	// Partials must not carry layout chrome.
	for _, banned := range []string{"<nav", "sse-status", "htmx.org"} {
		if strings.Contains(body, banned) {
			t.Fatalf("workflows partial contains layout chrome %q", banned)
		}
	}
}

func TestActionsRunsPartial(t *testing.T) {
	srv, _, _ := actionsServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getActionsFragment(t, srv, sid, "/partials/projects/proj/actions/runs?owner=acme&repo=app")
	for _, want := range []string{"Deploy run", "workflow_dispatch", "GitHub"} {
		if !strings.Contains(body, want) {
			t.Fatalf("runs partial missing %q:\n%s", want, body)
		}
	}
}

func TestActionsPageLockedBranchSelect(t *testing.T) {
	srv, cfg, _ := actionsServer(t)
	if err := cfg.SetProjectActionsRule("proj", config.ActionsDispatchRule{
		Workflow: "deploy.yml",
		Branches: []string{"production"},
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getActionsFragment(t, srv, sid, "/partials/projects/proj/actions/workflows?owner=acme&repo=app")
	// Locked form: only production in the deploy workflow's branch select.
	if !strings.Contains(body, "locked · production") {
		t.Fatalf("missing lock chip:\n%s", body)
	}
	// Find the deploy dispatch form's select and ensure main is not an option there.
	// The page may still mention main in the runs table — only the locked select matters.
	idx := strings.Index(body, `name="workflow" value=".github/workflows/deploy.yml"`)
	if idx < 0 {
		t.Fatal("deploy dispatch form missing")
	}
	formSlice := body[idx:]
	if end := strings.Index(formSlice, "</form>"); end >= 0 {
		formSlice = formSlice[:end]
	}
	if !strings.Contains(formSlice, `value="production"`) {
		t.Fatalf("locked select missing production:\n%s", formSlice)
	}
	if strings.Contains(formSlice, `value="main"`) || strings.Contains(formSlice, `value="feature"`) {
		t.Fatalf("locked select offered unlocked branches:\n%s", formSlice)
	}
}

func TestActionsDispatchHappyPath(t *testing.T) {
	srv, _, calls := actionsServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/actions/dispatch", sid, csrf, url.Values{
		"owner":     {"acme"},
		"repo":      {"app"},
		"workflow":  {".github/workflows/deploy.yml"},
		"ref":       {"main"},
		"input.env": {"prod"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") || !strings.Contains(loc, "/projects/proj/actions") {
		t.Fatalf("redirect=%q", loc)
	}
	call, ok := actionsDispatchCall(*calls)
	if !ok {
		t.Fatalf("workflow run not invoked: %+v", *calls)
	}
	joined := strings.Join(call.args, " ")
	for _, want := range []string{"run", "deploy.yml", "--ref", "main", "-f", "env=prod", "--repo", "acme/app"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, call.args)
		}
	}
	assertActionsDispatchAudit(t, srv, true)
}

func TestActionsDispatchLockedBranchRefuses(t *testing.T) {
	srv, cfg, calls := actionsServer(t)
	if err := cfg.SetProjectActionsRule("proj", config.ActionsDispatchRule{
		Workflow: "deploy.yml",
		Branches: []string{"production"},
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/actions/dispatch", sid, csrf, url.Values{
		"owner":    {"acme"},
		"repo":     {"app"},
		"workflow": {".github/workflows/deploy.yml"},
		"ref":      {"main"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if _, ok := actionsDispatchCall(*calls); ok {
		t.Fatal("gh workflow run must not run when ref is locked out")
	}
	assertActionsDispatchAudit(t, srv, false)
}

func TestActionsDispatchCapabilityRefuses(t *testing.T) {
	srv, cfg, calls := actionsServer(t)
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "investigator"); err != nil {
		t.Fatal(err)
	}
	if caps := cfg.ResolveCapabilities("proj", "member-1"); caps.GithubWrites {
		t.Fatalf("investigator must not have githubWrites: %+v", caps)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/actions/dispatch", sid, csrf, url.Values{
		"owner":    {"acme"},
		"repo":     {"app"},
		"workflow": {".github/workflows/deploy.yml"},
		"ref":      {"main"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if _, ok := actionsDispatchCall(*calls); ok {
		t.Fatal("gh workflow run must not run without capability")
	}
	assertActionsDispatchAudit(t, srv, false)
}

func TestActionsDispatchNonDispatchableRefuses(t *testing.T) {
	srv, _, calls := actionsServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/actions/dispatch", sid, csrf, url.Values{
		"owner":    {"acme"},
		"repo":     {"app"},
		"workflow": {".github/workflows/ci.yml"},
		"ref":      {"main"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want err redirect, got %q", loc)
	}
	decoded, _ := url.QueryUnescape(loc)
	if !strings.Contains(decoded, "not dispatchable") {
		t.Fatalf("want not-dispatchable message, got %q", loc)
	}
	if _, ok := actionsDispatchCall(*calls); ok {
		t.Fatal("must not dispatch a push-only workflow")
	}
	assertActionsDispatchAudit(t, srv, false)
}

func TestActionsSettingsAddRemoveRoundTrip(t *testing.T) {
	srv, cfg, _ := testServer(t)
	// Empty branches refused.
	w := postForm(t, srv, "/config/projects/actions-rule", url.Values{
		"name":     {"proj"},
		"workflow": {"deploy.yml"},
		"branches": {"  ,  "},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("empty branches should err: %q", loc)
	}
	// Add a rule.
	w = postForm(t, srv, "/config/projects/actions-rule", url.Values{
		"name":     {"proj"},
		"workflow": {"deploy.yml"},
		"branches": {"production, staging"},
		"repo":     {"app"},
	})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("unexpected err: %q", loc)
	}
	branches, locked := cfg.ActionsDispatchBranches("proj", "acme", "app", ".github/workflows/deploy.yml")
	if !locked || len(branches) != 2 || branches[0] != "production" {
		t.Fatalf("branches=%v locked=%v", branches, locked)
	}
	// Integrations tab renders the rule.
	body := getBody(t, srv.Handler(), "/config/projects/proj/integrations")
	if !strings.Contains(body, "GitHub Actions") || !strings.Contains(body, "deploy.yml") {
		t.Fatalf("integrations missing actions rules:\n%s", body)
	}
	if !strings.Contains(body, "production, staging") {
		t.Fatalf("branches not joined on settings page")
	}
	// Remove.
	w = postForm(t, srv, "/config/projects/actions-rule/remove", url.Values{
		"name":     {"proj"},
		"workflow": {"deploy.yml"},
		"repo":     {"app"},
	})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("remove err: %q", loc)
	}
	if _, locked := cfg.ActionsDispatchBranches("proj", "acme", "app", "deploy.yml"); locked {
		t.Fatal("rule survived remove")
	}
	assertAuditAction(t, srv, "config.set_project_actions_rule", true)
	assertAuditAction(t, srv, "config.remove_project_actions_rule", true)
}

func TestActionsPageAuthOffStillRendersChrome(t *testing.T) {
	// Matches TestPagesRender: no gh, no catalog — still shows page chrome.
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/projects/proj/actions")
	if !strings.Contains(body, `id="page-actions"`) {
		t.Fatal("page chrome missing")
	}
	assertNavActive(t, body, "Actions")
}

func TestRunBucketBadgeColors(t *testing.T) {
	// pending (in_progress) and pass (success) must not share a badge class —
	// live and status-done both paint green.
	cases := map[string]string{
		"pass":     "status-done",
		"fail":     "status-error",
		"pending":  "status-running",
		"skipping": "status-cancelled",
		"cancel":   "status-cancelled",
		"other":    "",
	}
	for bucket, want := range cases {
		if got := runBucketBadge(bucket); got != want {
			t.Errorf("runBucketBadge(%q)=%q want %q", bucket, got, want)
		}
	}
	if runBucketBadge("pass") == runBucketBadge("pending") {
		t.Fatal("pass and pending must use different badge classes")
	}
}

func TestActionsRunPageJobLogResponsive(t *testing.T) {
	srv, _, _ := actionsServer(t)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	// Shell: job card + lazy log region.
	body := getActionsFragment(t, srv, sid, "/projects/proj/actions/runs/99?owner=acme&repo=app")
	for _, want := range []string{
		`id="page-actions-run"`,
		`class="section card job-card"`,
		`id="job-log-1"`,
		`/actions/runs/99/job?job=1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page missing %q:\n%s", want, body)
		}
	}
	// Fragment: log body uses log-block so long lines stay inside the card.
	logBody := getActionsFragment(t, srv, sid, "/projects/proj/actions/runs/99/job?job=1&owner=acme&repo=app")
	for _, want := range []string{
		`class="mono log-block"`,
		`id="job-log-body-1"`,
		"ok",
	} {
		if !strings.Contains(logBody, want) {
			t.Fatalf("job log missing %q:\n%s", want, logBody)
		}
	}
	if !strings.Contains(logBody, strings.Repeat("x", 400)) {
		t.Fatal("job log body missing long line content")
	}
}
