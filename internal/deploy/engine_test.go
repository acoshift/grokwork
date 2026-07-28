package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// gitRepo builds a real git repo with a manifest committed at HEAD. The engine
// reads manifests through git plumbing, so a real repo is simpler and more
// honest here than stubbing every call.
func gitRepo(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, ".grokwork"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grokwork", "deploy.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "manifest")
	return dir
}

func engineConfig(t *testing.T, projectPath string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"discordToken":"tok","projects":{"app":{"path":"` + projectPath + `","allowedUserIds":["u1"]}},"channels":{"c1":"app"},"grokBin":"grok"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	cfg.DataDir = dir
	return &cfg
}

func testEngine(t *testing.T, manifest string) (*Engine, *config.Config, string) {
	t.Helper()
	repo := gitRepo(t, manifest)
	cfg := engineConfig(t, repo)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	// "*" so tests are not gated on primary-branch resolution; the ref gate has
	// its own tests below.
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{AllowedRefs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		eng.Stop(ctx)
	})
	return eng, cfg, repo
}

func builderCaps() config.Capabilities {
	return config.Capabilities{StartSessions: true, GithubWrites: true}
}

func waitTerminal(t *testing.T, eng *Engine, id string) Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, ok, err := eng.Store().Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok && r.Status.Terminal() {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a terminal status", id)
	return Run{}
}

const twoStepManifest = `version: 1
environments: [dev, prod]
services:
  api:
    dir: svc
    steps:
      - { name: one, run: "echo step-one > out1.txt" }
      - { name: two, run: "echo step-two" }
`

func TestEngineRunsStepsInCheckout(t *testing.T) {
	skipOnWindows(t)
	eng, _, repo := testEngine(t, twoStepManifest)
	// The service dir must exist in the commit for the step cwd to resolve.
	mustMkdirCommit(t, repo, "svc")

	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main",
		Actor: Actor{ID: "u1", Name: "Alice"}, Caps: builderCaps(),
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	got := waitTerminal(t, eng, run.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("status = %q error=%q steps=%+v", got.Status, got.Error, got.Steps)
	}
	for i, s := range got.Steps {
		if s.Status != StatusSucceeded {
			t.Fatalf("step %d = %+v", i, s)
		}
	}
	chunk, _, err := eng.Store().ReadStepLog(run.ID, 1, "two", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chunk), "step-two") {
		t.Fatalf("step log = %q", chunk)
	}
	// The ephemeral checkout is removed once the run is terminal.
	if got.CheckoutPath != "" {
		if _, err := os.Stat(got.CheckoutPath); !os.IsNotExist(err) {
			t.Fatalf("checkout %s survived", got.CheckoutPath)
		}
	}
}

// mustMkdirCommit adds a directory (with a placeholder) to the repo's HEAD.
func mustMkdirCommit(t *testing.T, repo, dir string) {
	t.Helper()
	full := filepath.Join(repo, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, ".keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "svc"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestEngineFailingStepSkipsTheRest(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: boom, run: "exit 7" }
      - { name: never, run: "echo nope" }
`
	eng, _, repo := testEngine(t, manifest)
	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main",
		Caps: builderCaps(),
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	got := waitTerminal(t, eng, run.ID)
	if got.Status != StatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Steps[0].ExitCode != 7 {
		t.Fatalf("exit = %d", got.Steps[0].ExitCode)
	}
	// Fail-fast: the later step must be visibly skipped, not silently absent.
	if got.Steps[1].Status != StatusSkipped {
		t.Fatalf("step 2 = %q, want skipped", got.Steps[1].Status)
	}
}

// TestEngineSecondRunOnBusyLaneQueues: a busy lane queues rather than refusing.
// Slice 4 rejected here; queueing is the designed behaviour.
func TestEngineSecondRunOnBusyLaneQueues(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 5" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps()}
	first, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitRunning(t, eng, first.ID)
	// A second trigger of the SAME commit is deduped by design, so move the
	// branch on: this one must queue behind the active run, not merge into it.
	mustEmptyCommit(t, repo)
	second, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatalf("second trigger err = %v, want it queued", err)
	}
	if second.ID == first.ID {
		t.Fatal("second trigger merged into the first")
	}
	if second.Status != StatusPending {
		t.Fatalf("second status = %q, want pending", second.Status)
	}
	_ = eng.Cancel(first.ID)
	waitTerminal(t, eng, first.ID)
}

// mustEmptyCommit moves the branch on so a later trigger sees a new SHA.
func mustEmptyCommit(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "-c", "user.email=t@e.com", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "move")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
}

// waitStepRunning waits until a specific step is executing, which is later than
// StatusRunning: the run is marked running before its checkout is created.
func waitStepRunning(t *testing.T, eng *Engine, id string, idx int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok, _ := eng.Store().Get(id); ok {
			if r.Status.Terminal() {
				t.Fatalf("run finished (%s: %s) before step %d started", r.Status, r.Error, idx)
			}
			if idx < len(r.Steps) && r.Steps[idx].Status == StatusRunning {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("step %d of %s never started", idx, id)
}

func waitRunning(t *testing.T, eng *Engine, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok, _ := eng.Store().Get(id); ok && r.Status == StatusRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s never started", id)
}

func TestEngineCancel(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 30" }
`
	eng, _, repo := testEngine(t, manifest)
	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStepRunning(t, eng, run.ID, 0)
	if err := eng.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	got := waitTerminal(t, eng, run.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q err=%q steps=%+v", got.Status, got.Error, got.Steps)
	}
}

func TestEngineRefusesWithoutCapability(t *testing.T) {
	skipOnWindows(t)
	eng, cfg, repo := testEngine(t, twoStepManifest)
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{
		RequireCapability: "approve", AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main",
		Caps: builderCaps(), // builder-class, but this env demands approve
	})
	if err == nil {
		t.Fatal("builder deployed to an approve-gated environment")
	}
}

func TestEngineRefusesWhenDisabled(t *testing.T) {
	skipOnWindows(t)
	eng, cfg, repo := testEngine(t, twoStepManifest)
	if err := cfg.SetProjectDeployEnabled("app", false); err != nil {
		t.Fatal(err)
	}
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	if err == nil {
		t.Fatal("deployed with the project's deploys disabled")
	}
}

// TestEngineRefusesUnallowedRef is the control that matters: it is what stops a
// pushed branch rewriting the pipeline and reaching production credentials.
func TestEngineRefusesUnallowedRef(t *testing.T) {
	skipOnWindows(t)
	eng, cfg, repo := testEngine(t, twoStepManifest)
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{
		AllowedRefs: []string{"main"},
	}); err != nil {
		t.Fatal(err)
	}
	// Create a side branch and try to deploy it.
	cmd := exec.Command("git", "-C", repo, "branch", "sneaky")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("branch: %v\n%s", err, out)
	}
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "sneaky", Caps: builderCaps(),
	})
	if err == nil {
		t.Fatal("deployed a ref outside the allowlist")
	}
	if !strings.Contains(err.Error(), "not an allowed ref") {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineRefusesUnknownEnvironment(t *testing.T) {
	skipOnWindows(t)
	eng, _, repo := testEngine(t, twoStepManifest)
	// prod is in the manifest but not configured in config.json.
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "prod", Ref: "main", Caps: builderCaps(),
	})
	if err == nil {
		t.Fatal("deployed to an environment with no configured policy")
	}
}

func TestEngineRefusesWhileStopping(t *testing.T) {
	skipOnWindows(t)
	eng, _, repo := testEngine(t, twoStepManifest)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	eng.Stop(ctx)
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	// A trigger landing during shutdown must be refused, or it starts work
	// nothing waits on and nothing kills.
	if err == nil {
		t.Fatal("accepted a trigger while shutting down")
	}
}

func TestEngineDedupesSameShaWithinWindow(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 5" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main",
		Actor: Actor{ID: "u1", Name: "Alice"}, Caps: builderCaps()}
	first, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// A different actor clicking the same cell must land on the same run, not
	// queue a second identical prod deploy.
	req.Actor = Actor{ID: "u2", Name: "Bob"}
	second, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatalf("duplicate trigger errored instead of deduping: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second run %s != first %s; the dedupe keyed on the actor", second.ID, first.ID)
	}
	_ = eng.Cancel(first.ID)
	waitTerminal(t, eng, first.ID)
}

// TestEngineStopMarksInterrupted pins that a deploy is never left "running" in
// the record after a restart, and never auto-resumed.
func TestEngineStopMarksInterrupted(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 30" }
`
	eng, _, repo := testEngine(t, manifest)
	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, eng, run.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	eng.Stop(ctx)

	got, ok, err := eng.Store().Get(run.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !got.Status.Terminal() {
		t.Fatalf("status = %q, want a terminal status after Stop", got.Status)
	}
	if got.PID != 0 {
		t.Fatalf("PID %d left on a terminal run", got.PID)
	}
}

func TestRefAllowed(t *testing.T) {
	cases := []struct {
		ref     string
		allowed []string
		want    bool
	}{
		{"main", []string{"main"}, true},
		{"origin/main", []string{"main"}, true},
		{"refs/heads/main", []string{"main"}, true},
		{"main", []string{"origin/main"}, true},
		{"sneaky", []string{"main"}, false},
		{"anything", []string{"*"}, true},
		{"release/1.2", []string{"release/*"}, true},
		{"releases/1.2", []string{"release/*"}, false},
		{"main", nil, false},
	}
	for _, tc := range cases {
		if got := RefAllowed(tc.ref, tc.allowed); got != tc.want {
			t.Errorf("RefAllowed(%q, %v) = %v, want %v", tc.ref, tc.allowed, got, tc.want)
		}
	}
}

func TestCapabilityAllows(t *testing.T) {
	builder := config.Capabilities{StartSessions: true, GithubWrites: true}
	approver := config.Capabilities{StartSessions: true, GithubWrites: true, Approve: true}
	investigator := config.Capabilities{Investigate: true}

	if !CapabilityAllows(builder, "") {
		t.Error("builder refused by the default gate")
	}
	if CapabilityAllows(investigator, "") {
		t.Error("investigator passed the default builder gate")
	}
	if CapabilityAllows(builder, "approve") {
		t.Error("builder passed an approve gate")
	}
	if !CapabilityAllows(approver, "approve") {
		t.Error("approver refused by an approve gate")
	}
	// An unknown requirement must fail closed rather than degrade to builder.
	if CapabilityAllows(approver, "wizard") {
		t.Error("unknown capability did not fail closed")
	}
}

func TestEngineQueuesSecondRun(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 2" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps()}
	first, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, eng, first.ID)
	mustEmptyCommit(t, repo)

	second, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatalf("second Trigger: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("second trigger was deduped into the first")
	}
	if second.Status != StatusPending {
		t.Fatalf("second status = %q, want pending", second.Status)
	}
	lane := LaneKey("app", "", "api", "dev")
	if n := eng.QueueLen(lane); n != 1 {
		t.Fatalf("QueueLen = %d, want 1", n)
	}
	// The queued run is promoted once the active one finishes.
	waitTerminal(t, eng, first.ID)
	got := waitTerminal(t, eng, second.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("promoted run status = %q err=%q", got.Status, got.Error)
	}
}

func TestEngineQueueCapRejects(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 20" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps()}
	first, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, eng, first.ID)
	for i := range MaxQueuePerLane {
		mustEmptyCommit(t, repo)
		if _, err := eng.Trigger(context.Background(), req); err != nil {
			t.Fatalf("queue %d: %v", i, err)
		}
	}
	mustEmptyCommit(t, repo)
	if _, err := eng.Trigger(context.Background(), req); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull past the cap", err)
	}
	_ = eng.Cancel(first.ID)
}

// TestEngineCancelQueuedRun: a queued run has no goroutine, so cancelling it
// must mark it terminal and drop it from the lane, or promotion would later
// start something the operator already stopped.
func TestEngineCancelQueuedRun(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: slow, run: "sleep 3" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps()}
	first, _ := eng.Trigger(context.Background(), req)
	waitRunning(t, eng, first.ID)
	mustEmptyCommit(t, repo)
	second, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := eng.Store().Get(second.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("queued run status = %q, want cancelled", got.Status)
	}
	lane := LaneKey("app", "", "api", "dev")
	if n := eng.QueueLen(lane); n != 0 {
		t.Fatalf("QueueLen = %d after cancelling the queued run", n)
	}
	_ = eng.Cancel(first.ID)
	waitTerminal(t, eng, first.ID)
	// The cancelled run must stay cancelled, not be promoted.
	again, _, _ := eng.Store().Get(second.ID)
	if again.Status != StatusCancelled {
		t.Fatalf("cancelled run was promoted: %q", again.Status)
	}
}

func TestRecoverAtStartupMarksInterrupted(t *testing.T) {
	eng, _, _ := testEngine(t, twoStepManifest)
	running, err := eng.SeedRunForTest(Run{
		Project: "app", Service: "api", Env: "dev", Status: StatusRunning,
		Steps: []StepRecord{{Name: "a", Status: StatusRunning}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := eng.SeedRunForTest(Run{
		Project: "app", Service: "api", Env: "prod", Status: StatusPending,
		Steps: []StepRecord{{Name: "a", Status: StatusPending}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	interrupted, cancelled, blocked := eng.RecoverAtStartup()
	if interrupted != 1 || cancelled != 1 || blocked != 0 {
		t.Fatalf("recovered i=%d c=%d b=%d, want 1/1/0", interrupted, cancelled, blocked)
	}
	got, _, _ := eng.Store().Get(running.ID)
	if got.Status != StatusInterrupted {
		t.Fatalf("running run = %q, want interrupted (never auto-resumed)", got.Status)
	}
	got, _, _ = eng.Store().Get(pending.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("pending run = %q, want cancelled", got.Status)
	}
}

// TestRecoverAtStartupBlocksLiveGroup: a run whose process group outlived the
// restart must block the lane rather than be marked interrupted, and the pid
// must NOT be signalled — it may have been recycled.
func TestRecoverAtStartupBlocksLiveGroup(t *testing.T) {
	skipOnWindows(t)
	// A real child in its own process group — the actual orphan shape. Our own
	// pid would not do: it is not a process-group leader, so there is no group
	// with that id and the probe correctly finds nothing.
	child := exec.Command("sh", "-c", "sleep 30")
	setProcessGroup(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := child.Process.Pid
	t.Cleanup(func() {
		killProcessGroupHard(pgid, 100*time.Millisecond)
		_ = child.Wait()
	})
	if !processGroupAlive(pgid) {
		t.Fatal("child group is not alive; the test cannot exercise the branch")
	}

	eng, _, _ := testEngine(t, twoStepManifest)
	run, err := eng.SeedRunForTest(Run{
		Project: "app", Service: "api", Env: "dev", Status: StatusRunning, PID: pgid,
		Steps: []StepRecord{{Name: "a", Status: StatusRunning}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, blocked := eng.RecoverAtStartup()
	if blocked != 1 {
		t.Fatalf("blocked = %d, want 1", blocked)
	}
	got, _, _ := eng.Store().Get(run.ID)
	if got.Status != StatusBlocked {
		t.Fatalf("status = %q, want blocked", got.Status)
	}
	if !strings.Contains(got.Error, "not killed") {
		t.Fatalf("error does not explain the no-kill policy: %q", got.Error)
	}
}

func TestRecoverAtStartupSkipsOtherHosts(t *testing.T) {
	eng, _, _ := testEngine(t, twoStepManifest)
	run, err := eng.SeedRunForTest(Run{
		Project: "app", Service: "api", Env: "dev", Status: StatusRunning,
		Host:  "some-other-machine",
		Steps: []StepRecord{{Name: "a", Status: StatusRunning}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.RecoverAtStartup()
	got, _, _ := eng.Store().Get(run.ID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %q; another host's run must not be touched", got.Status)
	}
}

func TestSweepOrphanCheckouts(t *testing.T) {
	eng, _, _ := testEngine(t, twoStepManifest)
	orphan := filepath.Join(eng.root, "checkouts", "app", "d_"+strings.Repeat("a", 32))
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	live, err := eng.SeedRunForTest(Run{
		Project: "app", Service: "api", Env: "dev", Status: StatusRunning,
		Host:  "some-other-machine", // survives recovery so its checkout is "live"
		Steps: []StepRecord{{Name: "a", Status: StatusRunning}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	liveDir := filepath.Join(eng.root, "checkouts", "app", live.ID)
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	eng.RecoverAtStartup()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan checkout survived; nothing else ever cleans these up")
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatal("a live run's checkout was swept")
	}
}

// TestRedeployReplaysFrozenStepsAtSameSHA: the point of redeploy is exact
// reproduction, so it must not re-read the manifest — the branch may have moved.
func TestRedeployReplaysFrozenStepsAtSameSHA(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: original, run: "echo original" }
`
	eng, _, repo := testEngine(t, manifest)
	req := TriggerRequest{Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps()}
	first, err := eng.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitTerminal(t, eng, first.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("first run %q: %s", got.Status, got.Error)
	}

	// Rewrite the pipeline and move the branch on.
	if err := os.WriteFile(filepath.Join(repo, ".grokwork", "deploy.yaml"), []byte(`version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: rewritten, run: "echo rewritten" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "-c", "user.email=t@e.com", "-c", "user.name=t", "commit", "-qam", "rewrite")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	redeploy, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev",
		Caps: builderCaps(), RedeployOf: first.ID,
	})
	if err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if redeploy.SHA != first.SHA {
		t.Fatalf("redeploy SHA = %s, want the original %s", redeploy.SHA, first.SHA)
	}
	if len(redeploy.Steps) != 1 || redeploy.Steps[0].Name != "original" {
		t.Fatalf("redeploy did not replay the frozen steps: %+v", redeploy.Steps)
	}
	if redeploy.RedeployOf != first.ID {
		t.Fatalf("RedeployOf = %q", redeploy.RedeployOf)
	}
	waitTerminal(t, eng, redeploy.ID)
}

// TestRedeployWaivesRefCheckOnSameLane: re-asserting reachability would block
// rollback during an incident, and the commit already passed the gate here.
func TestRedeployWaivesRefCheckOnSameLane(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: ok, run: "true" }
`
	eng, cfg, repo := testEngine(t, manifest)
	first, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, eng, first.ID)

	// Tighten the allowlist to something the original ref no longer matches.
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{
		AllowedRefs: []string{"release/*"},
	}); err != nil {
		t.Fatal(err)
	}
	// A fresh trigger is refused...
	if _, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	}); err == nil {
		t.Fatal("fresh trigger passed a tightened allowlist")
	}
	// ...but the rollback path stays open.
	redeploy, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev",
		Caps: builderCaps(), RedeployOf: first.ID,
	})
	if err != nil {
		t.Fatalf("redeploy refused during what would be an incident: %v", err)
	}
	if redeploy.RefCheck != "waived_redeploy" {
		t.Fatalf("RefCheck = %q, want it recorded as waived", redeploy.RefCheck)
	}
	waitTerminal(t, eng, redeploy.ID)
}

// TestRedeployStillChecksCapability: waiving the ref check must not waive the
// gate that says who may touch the environment.
func TestRedeployStillChecksCapability(t *testing.T) {
	skipOnWindows(t)
	eng, cfg, repo := testEngine(t, twoStepManifest)
	mustMkdirCommit(t, repo, "svc")
	first, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev", Ref: "main", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, eng, first.ID)
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{
		RequireCapability: "approve", AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "dev",
		Caps: builderCaps(), RedeployOf: first.ID,
	}); err == nil {
		t.Fatal("redeploy bypassed the capability gate")
	}
}

// cloneRepo makes `origin` a real remote, so remote-tracking refs behave the way
// they do in production: they move only on fetch.
func cloneRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "-q", origin, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	return dir
}

func headSHA(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestTriggerFetchesBeforeResolvingRef is the staleness bug.
//
// PrimaryStartRef resolves through origin/<primary>, a remote-tracking ref that
// only moves on fetch. Without a fetch at trigger time, "deploy main" deploys
// whatever the last background fetch saw — stale by up to
// repoFetchIntervalMinutes, and unbounded when an operator disables idle fetch.
// A pull is never needed: the local branch is not consulted at all.
func TestTriggerFetchesBeforeResolvingRef(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: ok, run: "true" }
`
	origin := gitRepo(t, manifest)
	clone := cloneRepo(t, origin)

	cfg := engineConfig(t, clone)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{AllowedRefs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		eng.Stop(ctx)
	})

	// Someone pushes to the origin. The clone has not fetched, so its
	// origin/main still points at the old commit.
	mustEmptyCommit(t, origin)
	want := headSHA(t, origin)
	stale, err := gitworktree.ResolveRefSHA(context.Background(), clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if stale == want {
		t.Fatal("clone already saw the new commit; the test cannot detect a missing fetch")
	}

	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "dev", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if run.SHA != want {
		t.Fatalf("deployed %s, want the current remote tip %s (trigger did not fetch)", run.SHA[:7], want[:7])
	}
	waitTerminal(t, eng, run.ID)
}

// TestRedeployDoesNotNeedTheNetwork: a redeploy replays a pinned SHA, so it must
// not depend on a reachable remote — that is exactly when a rollback is needed.
func TestRedeployDoesNotNeedTheNetwork(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [dev]
services:
  api:
    steps:
      - { name: ok, run: "true" }
`
	origin := gitRepo(t, manifest)
	clone := cloneRepo(t, origin)
	cfg := engineConfig(t, clone)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{AllowedRefs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		eng.Stop(ctx)
	})
	first, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "dev", Caps: builderCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, eng, first.ID)

	// Break the remote entirely.
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	again, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "dev",
		Caps: builderCaps(), RedeployOf: first.ID,
	})
	if err != nil {
		t.Fatalf("redeploy needed the remote: %v", err)
	}
	if again.SHA != first.SHA {
		t.Fatalf("redeploy SHA = %s, want the pinned %s", again.SHA, first.SHA)
	}
	got := waitTerminal(t, eng, again.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("redeploy status = %q: %s", got.Status, got.Error)
	}
}

func gatedEngine(t *testing.T) (*Engine, *config.Config, string, string) {
	t.Helper()
	manifest := `version: 1
environments: [dev, prod]
services:
  api:
    steps:
      - { name: ok, run: "true" }
`
	origin := gitRepo(t, manifest)
	clone := cloneRepo(t, origin)
	cfg := engineConfig(t, clone)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	// prod is gated; dev is not — the drift check applies only to the former.
	if err := cfg.SetProjectDeployEnvPolicy("app", "prod", config.DeployEnvPolicy{
		RequireCapability: "approve", AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "dev", config.DeployEnvPolicy{
		AllowedRefs: []string{"*"},
	}); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		eng.Stop(ctx)
	})
	return eng, cfg, origin, clone
}

func approverCaps() config.Capabilities {
	return config.Capabilities{StartSessions: true, GithubWrites: true, Approve: true}
}

// TestGatedDeployRefusesWhenTheRefMoved is the drift guard. The trigger fetches
// before resolving, so a protected environment must not silently ship a commit
// the operator never saw.
func TestGatedDeployRefusesWhenTheRefMoved(t *testing.T) {
	skipOnWindows(t)
	eng, _, origin, clone := gatedEngine(t)

	// What the page showed.
	shown, err := gitworktree.ResolveRefSHA(context.Background(), clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	// Someone pushes between page load and click.
	mustEmptyCommit(t, origin)
	moved := headSHA(t, origin)

	_, err = eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "prod",
		Caps: approverCaps(), ExpectSHA: shown,
	})
	if err == nil {
		t.Fatal("gated deploy shipped a commit the operator never reviewed")
	}
	for _, want := range []string{"moved", shortSHAOf(shown), shortSHAOf(moved), "reload"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestGatedDeployProceedsWhenTheRefIsUnchanged(t *testing.T) {
	skipOnWindows(t)
	eng, _, _, clone := gatedEngine(t)
	shown, err := gitworktree.ResolveRefSHA(context.Background(), clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "prod",
		Caps: approverCaps(), ExpectSHA: shown,
	})
	if err != nil {
		t.Fatalf("gated deploy refused an unchanged ref: %v", err)
	}
	if run.SHA != shown {
		t.Fatalf("deployed %s, want the reviewed %s", run.SHA, shown)
	}
	waitTerminal(t, eng, run.ID)
}

// TestGatedDeployRequiresAnExpectation is the fail-closed half: a caller that
// omits the expectation must be refused, not silently exempted.
func TestGatedDeployRequiresAnExpectation(t *testing.T) {
	skipOnWindows(t)
	eng, _, _, clone := gatedEngine(t)
	_, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "prod",
		Caps: approverCaps(), // no ExpectSHA
	})
	if err == nil {
		t.Fatal("gated deploy accepted with no reviewed commit")
	}
	if !strings.Contains(err.Error(), "confirmed") {
		t.Fatalf("error = %v", err)
	}
}

// TestUngatedDeployIgnoresDrift: for dev, deploying the current tip is the
// intent, so a moved ref must not add friction.
func TestUngatedDeployIgnoresDrift(t *testing.T) {
	skipOnWindows(t)
	eng, _, origin, clone := gatedEngine(t)
	shown, err := gitworktree.ResolveRefSHA(context.Background(), clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	mustEmptyCommit(t, origin)
	moved := headSHA(t, origin)

	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "dev",
		Caps: builderCaps(), ExpectSHA: shown,
	})
	if err != nil {
		t.Fatalf("ungated deploy refused: %v", err)
	}
	if run.SHA != moved {
		t.Fatalf("deployed %s, want the current tip %s", run.SHA, moved)
	}
	waitTerminal(t, eng, run.ID)

	// And with no expectation at all.
	mustEmptyCommit(t, origin)
	if _, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "dev", Caps: builderCaps(),
	}); err != nil {
		t.Fatalf("ungated deploy required an expectation: %v", err)
	}
}

// TestGatedRedeployNeedsNoExpectation: a redeploy is SHA-pinned, so there is
// nothing to drift — and blocking it would break rollback, which is the one
// thing a gated environment needs most during an incident.
func TestGatedRedeployNeedsNoExpectation(t *testing.T) {
	skipOnWindows(t)
	eng, _, origin, clone := gatedEngine(t)
	shown, err := gitworktree.ResolveRefSHA(context.Background(), clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "prod",
		Caps: approverCaps(), ExpectSHA: shown,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, eng, first.ID)

	// The branch moves on; a rollback must still be possible with no expectation.
	mustEmptyCommit(t, origin)
	again, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: clone, Service: "api", Env: "prod",
		Caps: approverCaps(), RedeployOf: first.ID,
	})
	if err != nil {
		t.Fatalf("gated redeploy refused: %v", err)
	}
	if again.SHA != first.SHA {
		t.Fatalf("redeploy SHA = %s, want the pinned %s", again.SHA, first.SHA)
	}
	waitTerminal(t, eng, again.ID)
}
