package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("steps run under sh")
	}
}

func runStep(t *testing.T, ctx context.Context, spec StepSpec, secrets []string) (StepResult, string) {
	t.Helper()
	if spec.Dir == "" {
		spec.Dir = t.TempDir()
	}
	if spec.Name == "" {
		spec.Name = "step"
	}
	var buf bytes.Buffer
	res := RunStep(ctx, spec, &buf, secrets, nil)
	return res, buf.String()
}

func TestRunStepCapturesOutputAndExitCode(t *testing.T) {
	skipOnWindows(t)
	res, log := runStep(t, context.Background(), StepSpec{
		Command: "echo out; echo err >&2; exit 3",
	}, nil)
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.OK() {
		t.Fatal("OK() true for a failing step")
	}
	// stdout and stderr are merged: a deploy log that separates them loses the
	// ordering that explains what happened.
	if !strings.Contains(log, "out") || !strings.Contains(log, "err") {
		t.Fatalf("log missing streams: %q", log)
	}
	if res.LogBytes == 0 {
		t.Fatal("LogBytes not recorded")
	}
}

func TestRunStepSucceeds(t *testing.T) {
	skipOnWindows(t)
	res, _ := runStep(t, context.Background(), StepSpec{Command: "true"}, nil)
	if !res.OK() || res.ExitCode != 0 {
		t.Fatalf("res = %+v, want success", res)
	}
}

func TestRunStepRunsInDir(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, log := runStep(t, context.Background(), StepSpec{Command: "ls", Dir: dir}, nil)
	if !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(log, "marker") {
		t.Fatalf("step did not run in Dir: %q", log)
	}
}

// TestRunStepTimeoutKillsGrandchild is the flagship case.
//
// The grandchild TRAPS SIGTERM, which is the case grokrun's KillProcessGroup
// gets wrong: it group-SIGTERMs (killing well-behaved children) but then polls
// only the leader and returns as soon as that exits, so its group SIGKILL never
// fires and a trapping grandchild survives. Build tools trap TERM to clean up,
// and a timed-out `docker build` must not leave one running.
func TestRunStepTimeoutKillsGrandchild(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	stubborn := filepath.Join(dir, "stubborn.sh")
	if err := os.WriteFile(stubborn, []byte("#!/bin/sh\ntrap '' TERM\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The leader exits on SIGTERM; only a group SIGKILL can reach the sleeper.
	res, _ := runStep(t, context.Background(), StepSpec{
		Command:   stubborn + " & echo $! > " + pidFile + "; sleep 60",
		Dir:       dir,
		TimeoutMs: 400,
		// Shorten the escalation so the test is not gated on the production grace.
		KillGraceMs: 300,
	}, nil)

	if !res.TimedOut || res.ExitCode != ExitTimeout {
		t.Fatalf("res = %+v, want a timeout with exit %d", res, ExitTimeout)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("grandchild never recorded its pid: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("bad pid %q: %v", raw, err)
	}
	// Give the kill a moment to land, then assert the grandchild is gone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Do not leak a sleeper into the developer's session if the test fails.
	killProcessGroupHard(pid, 0)
	t.Fatalf("grandchild %d survived the step timeout", pid)
}

func TestRunStepCancelPropagates(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, _ := runStep(t, ctx, StepSpec{Command: "sleep 60", TimeoutMs: 60_000}, nil)
	if !res.Cancelled || res.ExitCode != ExitCancelled {
		t.Fatalf("res = %+v, want cancelled with exit %d", res, ExitCancelled)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancel took %s; the group kill did not land", elapsed)
	}
}

// TestRunStepReaderJoinedWhenGrandchildHoldsPipe pins that a surviving
// grandchild holding the pipe's write end cannot wedge the runner, and that the
// reader is joined before the log is finalised (run with -race).
func TestRunStepReaderJoinedWhenGrandchildHoldsPipe(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "holder.pid")
	start := time.Now()
	// The backgrounded sleeper inherits stdout, so the pipe stays open after the
	// shell exits. cmd.Wait must still return, and the runner must not block
	// past the reader grace.
	res, log := runStep(t, context.Background(), StepSpec{
		Command: "sh -c 'sleep 30 & echo $! > " + pidFile + "; echo done'",
		Dir:     dir,
	}, nil)
	elapsed := time.Since(start)

	if !strings.Contains(log, "done") {
		t.Fatalf("output lost: %q", log)
	}
	if elapsed > readerGrace+8*time.Second {
		t.Fatalf("runner blocked for %s on a grandchild holding the pipe", elapsed)
	}
	if res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if raw, err := os.ReadFile(pidFile); err == nil {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err == nil {
			killProcessGroupHard(pid, 0)
		}
	}
}

// TestRunStepRedactsSecretsInLog proves redaction happens before bytes land,
// so every surface built from the log inherits it.
func TestRunStepRedactsSecretsInLog(t *testing.T) {
	skipOnWindows(t)
	const secret = "s3cr3t-token-value"
	res, log := runStep(t, context.Background(), StepSpec{
		Command: "echo \"token is " + secret + " ok\"",
	}, []string{secret})
	if !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(log, secret) {
		t.Fatalf("secret reached the log: %q", log)
	}
	if !strings.Contains(log, "••••") {
		t.Fatalf("no redaction marker: %q", log)
	}
	// Surrounding output survives: redaction must not blank the whole line.
	if !strings.Contains(log, "token is") || !strings.Contains(log, "ok") {
		t.Fatalf("redaction ate the context: %q", log)
	}
}

// TestRunStepRedactsEnvDump is the concrete scenario: a step that runs `env`
// (or `set -x`) would otherwise write every credential into a log the web UI
// renders.
func TestRunStepRedactsEnvDump(t *testing.T) {
	skipOnWindows(t)
	const secret = "kube-token-abcdef123456"
	env := BuildEnv(RunVars{Project: "p", Service: "s", Env: "prod"}, map[string]string{"K8S_TOKEN": secret})
	res, log := runStep(t, context.Background(), StepSpec{Command: "env", Env: env}, []string{secret})
	if !res.OK() {
		t.Fatalf("res = %+v log=%q", res, log)
	}
	if strings.Contains(log, secret) {
		t.Fatalf("env dump leaked the secret:\n%s", log)
	}
	// The variable name is still visible, which is what makes a failure debuggable.
	if !strings.Contains(log, "K8S_TOKEN") {
		t.Fatalf("env dump lost the variable name:\n%s", log)
	}
}

func TestRunStepStartFailureIsReported(t *testing.T) {
	skipOnWindows(t)
	res, _ := runStep(t, context.Background(), StepSpec{
		Command: "true",
		Dir:     filepath.Join(t.TempDir(), "does-not-exist"),
	}, nil)
	if res.OK() || res.Err == "" {
		t.Fatalf("res = %+v, want a start failure", res)
	}
}

func TestStepDirContainment(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "services", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := StepDir(checkout, "services/api")
	if err != nil {
		t.Fatalf("StepDir: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("services", "api")) {
		t.Fatalf("StepDir = %q", got)
	}
	if _, err := StepDir(checkout, "../escape"); err == nil {
		t.Fatal("StepDir accepted a parent escape")
	}
	if _, err := StepDir(checkout, "/etc"); err == nil {
		t.Fatal("StepDir accepted an absolute path")
	}
	// Empty and "." both mean the checkout root.
	for _, d := range []string{"", "."} {
		if _, err := StepDir(checkout, d); err != nil {
			t.Fatalf("StepDir(%q) = %v", d, err)
		}
	}
}

// TestStepDirRefusesSymlinkEscape covers what parse-time validation cannot see:
// a relative, ..-free path inside the repo that is a symlink pointing out.
func TestStepDirRefusesSymlinkEscape(t *testing.T) {
	skipOnWindows(t)
	checkout := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(checkout, "sneaky")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := StepDir(checkout, "sneaky"); err == nil {
		t.Fatal("StepDir followed a symlink out of the checkout")
	}
}
