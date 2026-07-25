package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// TestEndToEndSafeFakeTarget is the plan's verification scenario: one service
// with an echo, a sleep, and a deliberate failure, plus a dummy credential that
// must be masked. Nothing real is touched.
func TestEndToEndSafeFakeTarget(t *testing.T) {
	skipOnWindows(t)
	manifest := `version: 1
environments: [sandbox]
services:
  api:
    steps:
      - { name: hello,  run: "echo hello; echo token=$FAKE_TOKEN" }
      - { name: settle, run: "sleep 1" }
      - { name: boom,   run: "exit 1" }
      - { name: never,  run: "echo should-not-run" }
`
	repo := gitRepo(t, manifest)
	cfg := engineConfig(t, repo)
	if err := cfg.SetProjectDeployEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectDeployEnvPolicy("app", "sandbox", config.DeployEnvPolicy{AllowedRefs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	const fakeToken = "fake-token-do-not-log-123456"
	if err := cfg.SetProjectDeployEnvVar("app", "sandbox", "FAKE_TOKEN", fakeToken, true); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{}
	eng.SetNotifier(n)

	run, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "sandbox", Ref: "main",
		Actor: Actor{ID: "u1", Name: "Alice"}, Caps: builderCaps(),
	})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	got := waitTerminal(t, eng, run.ID)

	if got.Status != StatusFailed {
		t.Fatalf("status = %q, want failed at the deliberate step", got.Status)
	}
	if got.Steps[0].Status != StatusSucceeded || got.Steps[2].Status != StatusFailed {
		t.Fatalf("step statuses = %+v", got.Steps)
	}
	if got.Steps[3].Status != StatusSkipped {
		t.Fatalf("step after the failure = %q, want skipped", got.Steps[3].Status)
	}
	// The credential must not be in the log the UI and Discord both read.
	chunk, _, err := eng.Store().ReadStepLogTail(run.ID, 0, "hello", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chunk), fakeToken) {
		t.Fatalf("credential reached the log:\n%s", chunk)
	}
	if !strings.Contains(string(chunk), "hello") || !strings.Contains(string(chunk), "••••") {
		t.Fatalf("log lost its context or its mask:\n%s", chunk)
	}
	// The ephemeral checkout is gone.
	if got.CheckoutPath != "" {
		if _, err := os.Stat(got.CheckoutPath); !os.IsNotExist(err) {
			t.Fatalf("checkout %s survived a terminal run", got.CheckoutPath)
		}
	}
	if _, err := os.Stat(filepath.Join(eng.root, "checkouts")); err == nil {
		// The tree may exist but must hold no run directories.
		eng.sweepOrphanCheckouts()
	}
	// The notice went out, names the failed step, and carries no credential.
	ch, _ := n.snapshot()
	if len(ch) != 1 {
		t.Fatalf("notices sent = %d, want 1", len(ch))
	}
	if !strings.Contains(ch[0], "boom") {
		t.Fatalf("notice does not name the failed step:\n%s", ch[0])
	}
	if strings.Contains(ch[0], fakeToken) {
		t.Fatalf("notice leaked the credential:\n%s", ch[0])
	}

	// Redeploy replays the same commit and the same frozen steps.
	again, err := eng.Trigger(context.Background(), TriggerRequest{
		Project: "app", RepoPath: repo, Service: "api", Env: "sandbox",
		Caps: builderCaps(), RedeployOf: run.ID,
	})
	if err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if again.SHA != run.SHA || len(again.Steps) != len(run.Steps) {
		t.Fatalf("redeploy diverged: sha=%s steps=%d", again.SHA, len(again.Steps))
	}
	waitTerminal(t, eng, again.ID)
}
