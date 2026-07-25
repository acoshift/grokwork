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

func TestEngineRejectsSecondRunOnSameLane(t *testing.T) {
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
	// branch on: this must be refused for the lane, not merged into the first.
	mustEmptyCommit(t, repo)
	_, err = eng.Trigger(context.Background(), req)
	if !errors.Is(err, ErrLaneBusy) {
		t.Fatalf("second trigger err = %v, want ErrLaneBusy", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
