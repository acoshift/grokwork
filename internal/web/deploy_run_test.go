package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/deploy"
)

func seedRun(t *testing.T, srv *Server, status deploy.Status, logBody string) deploy.Run {
	t.Helper()
	run, err := srv.Deploys().SeedRunForTest(deploy.Run{
		Project: "proj", Repo: "acme/api", Service: "api", Env: "prod",
		Ref: "main", SHA: "abcdef1234567890", ShortSHA: "abcdef1",
		Status: status, ActorName: "Alice", QueuedAt: "2026-07-20T10:00:00Z",
		Steps: []deploy.StepRecord{
			{Name: "build", Command: "docker build .", Status: deploy.StatusSucceeded, ExitCode: 0},
			{Name: "rollout", Command: "kubectl apply", Status: status},
		},
		CurrentStep: 1,
	}, map[int]string{1: logBody})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestDeployRunPageRenders(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "rolling out\n")

	body := getBody(t, srv.Handler(), "/projects/proj/deploys/"+run.ID)
	for _, want := range []string{
		`id="page-deploy-run"`,
		">api<", ">prod<", "abcdef1",
		">build<", ">rollout<",
		"rolling out",
		// The live region must carry the exact contract or an SSE swap wipes the page.
		`class="live-region"`, `hx-target="this"`, `hx-select="unset"`, `hx-trigger="sse:deploy"`,
		// The log itself renders inline, not only after the first SSE tick.
		`id="deploy-log"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page missing %q", want)
		}
	}
}

// TestRawLogLinkOptsOutOfBoost: the shell boosts every anchor into #live-root,
// so a text/plain response would render as a blank page.
func TestRawLogLinkOptsOutOfBoost(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusSucceeded, "done\n")
	body := getBody(t, srv.Handler(), "/projects/proj/deploys/"+run.ID)
	idx := strings.Index(body, "/log.txt")
	if idx < 0 {
		t.Fatal("no raw log link")
	}
	// Look at the anchor containing the link.
	start := strings.LastIndex(body[:idx], "<a ")
	end := strings.Index(body[idx:], ">")
	anchor := body[start : idx+end]
	if !strings.Contains(anchor, `hx-boost="false"`) {
		t.Fatalf("raw log link is boosted, so it would render blank: %s", anchor)
	}
}

func TestDeployRunLogFragmentShowsTail(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "line one\nline two\n")

	// The fragment re-renders whole, so a viewer arriving mid-run sees
	// everything in one request without any client-side offset bookkeeping.
	full := getBody(t, srv.Handler(), "/projects/proj/deploys/"+run.ID+"/log")
	if !strings.Contains(full, "line one") || !strings.Contains(full, "line two") {
		t.Fatalf("fragment incomplete: %s", full)
	}
}

// TestDeployLiveRegionURLHasNoBakedIndex pins the bug this design was rewritten
// to avoid. htmx bakes a live region's hx-get at render time and the region
// element itself is never replaced by an innerHTML swap, so a step index or byte
// offset baked into that URL is replayed unchanged on every SSE tick — the log
// would freeze on the first step and never advance.
func TestDeployLiveRegionURLHasNoBakedIndex(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "x\n")
	body := getBody(t, srv.Handler(), "/projects/proj/deploys/"+run.ID)

	i := strings.Index(body, `id="live-deploy-run"`)
	if i < 0 {
		t.Fatal("no live region")
	}
	start := strings.LastIndex(body[:i], "<div")
	end := i + strings.Index(body[i:], ">")
	region := body[start:end]
	if strings.Contains(region, "after=") {
		t.Fatalf("live region bakes a byte offset, so the tail can never advance: %s", region)
	}
	if strings.Contains(region, "step=") {
		t.Fatalf("live region bakes a step index, so it polls the wrong step after the run advances: %s", region)
	}
}

// TestDeployLogFollowsCurrentStep: with no step given, the endpoint tracks the
// run's current step, which is what lets the region URL stay static.
func TestDeployLogFollowsCurrentStep(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	r, err := srv.Deploys().SeedRunForTest(deploy.Run{
		Project: "proj", Service: "api", Env: "prod", Status: deploy.StatusRunning,
		ShortSHA: "abc1234", CurrentStep: 1,
		Steps: []deploy.StepRecord{
			{Name: "build", Status: deploy.StatusSucceeded},
			{Name: "rollout", Status: deploy.StatusRunning},
		},
	}, map[int]string{0: "FIRST-STEP-OUTPUT\n", 1: "SECOND-STEP-OUTPUT\n"})
	if err != nil {
		t.Fatal(err)
	}
	body := getBody(t, srv.Handler(), "/projects/proj/deploys/"+r.ID+"/log")
	if !strings.Contains(body, "SECOND-STEP-OUTPUT") {
		t.Fatalf("did not follow the current step: %s", body)
	}
	if strings.Contains(body, "FIRST-STEP-OUTPUT") {
		t.Fatalf("showed a stale step: %s", body)
	}
}

func TestDeployLogPartialHasNoChrome(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "x\n")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/projects/proj/deploys/"+run.ID+"/log?step=1&after=0", nil))
	body := w.Body.String()
	for _, banned := range []string{`id="sse-status"`, "<nav", "/static/htmx.min.js", `hx-ext="sse"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("live fragment leaked layout chrome: %q", banned)
		}
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestDeployRunPageRejectsForeignProject(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusSucceeded, "")
	// A run id is not a capability: it must not be readable through another
	// project's path.
	code, _ := getStatusBody(t, srv, "/projects/other/deploys/"+run.ID)
	if code == http.StatusOK {
		t.Fatal("run readable through a project it does not belong to")
	}
}

// TestPostDeployRequiresFeature: every write feature is fail-closed when web
// auth is off, so the trigger route must not even exist by default.
func TestPostDeployRequiresFeature(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	w := postForm(t, srv, "/projects/proj/deploys", url.Values{
		"service": {"api"}, "env": {"dev"}, "ref": {"main"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with the deploy feature off", w.Code)
	}
}

func TestPostDeployCancelRequiresFeature(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "")
	w := postForm(t, srv, "/projects/proj/deploys/"+run.ID+"/cancel", url.Values{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with the deploy feature off", w.Code)
	}
}

// TestDeployTriggerFormsOutsideLiveRegion is the blocker this design exists to
// avoid: a confirmed prod deploy inside a live region can be silently dropped
// when an SSE swap detaches the form while the modal is open.
func TestDeployTriggerFormsOutsideLiveRegion(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	run := seedRun(t, srv, deploy.StatusRunning, "")
	body := getBody(t, srv.Handler(), "/projects/proj/deploys/"+run.ID)

	regionStart := strings.Index(body, `class="live-region"`)
	if regionStart < 0 {
		t.Fatal("no live region on the run page")
	}
	cancelForm := strings.Index(body, "/cancel")
	if cancelForm < 0 {
		t.Fatal("no cancel form")
	}
	if cancelForm > regionStart {
		t.Fatal("the cancel form is inside the live region; an SSE swap can drop a confirmed click")
	}
}

// TestLiveRegionPausesWhileConfirming pins the belt-and-braces guard in the
// shared swap-pause hook.
func TestLiveRegionPausesWhileConfirming(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	if !strings.Contains(body, "paused while confirming") {
		t.Fatal("live-region swap is not paused while the confirm modal is open")
	}
}

// TestLiveRegionEditPauseIsContainmentOnly pins that SSE live-region refreshes
// do not freeze merely because focus is on an editable outside the region
// (session composer, nav search, filter selects, rails). Edit-pause requires
// the active control to be contained by the requesting live-region.
func TestLiveRegionEditPauseIsContainmentOnly(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	body := getBody(t, srv.Handler(), "/projects/proj/deploys")
	for _, want := range []string{
		"paused while editing",
		"paused while confirming",
		"function isEditingInside",
		"region.contains(el)",
		"isEditingInside(elt)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("layout swap-pause missing containment contract %q", want)
		}
	}
	// Global focus must not gate: the old isEditing() predicate is gone.
	if strings.Contains(body, "function isEditing(") {
		t.Fatal("global isEditing() still present; edit pause must be containment-scoped")
	}
	if strings.Contains(body, "if (isEditing())") {
		t.Fatal("edit pause still uses global isEditing()")
	}
}

func TestLiveRevsIncludesDeploy(t *testing.T) {
	srv := deploysServer(t, testDeployManifest)
	before := srv.computeLiveRevs()
	if before.Deploy == "" {
		t.Fatal("deploy domain has no fingerprint")
	}
	// A run that starts and finishes inside one tick must still move the rev, or
	// a passive viewer's board never refreshes for it.
	seedRun(t, srv, deploy.StatusSucceeded, "")
	after := srv.computeLiveRevs()
	if after.Deploy == before.Deploy {
		t.Fatal("deploy fingerprint did not change after a run was recorded")
	}
}
