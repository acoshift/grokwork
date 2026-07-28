package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Log caps per step. Head streams live; the tail is retained and flushed at the
// end so the bytes explaining a failure survive a chatty build.
const (
	StepLogHeadBytes = 512 << 10
	StepLogTailBytes = 1536 << 10
)

// readerGrace is how long the output reader is given to drain naturally after
// the child exits, before its read end is force-closed. A daemonized grandchild
// can hold the write end open forever, so this bounds the wait — it never skips
// the join.
const readerGrace = 2 * time.Second

// Exit codes for conditions that are not a command's own exit status. These
// mirror the shell convention already used for agent runs (internal/grokrun's
// contextResult), so a reader of one recognises the other.
const (
	ExitTimeout   = 124
	ExitCancelled = 130
)

// StepSpec is one resolved step, ready to run.
type StepSpec struct {
	Name string
	// Command runs under `sh -c`.
	Command string
	// Dir is the absolute working directory (checkout + the service's dir).
	Dir string
	// Env is the fully built environment (see BuildEnv). Never nil in practice:
	// an empty env would leave the command with no PATH.
	Env       []string
	TimeoutMs int
	// KillGraceMs is how long the process group gets to exit on SIGTERM before
	// SIGKILL. 0 uses DefaultKillGrace. Explicit rather than a package var so
	// tests can shorten it without racing the kill goroutine.
	KillGraceMs int
}

// StepResult is the outcome of one step.
type StepResult struct {
	Name      string
	ExitCode  int
	TimedOut  bool
	Cancelled bool
	// Err carries a failure to *start or supervise* the command, as opposed to
	// the command failing on its own terms.
	Err      string
	Elapsed  time.Duration
	LogBytes int64
	LogTrunc bool
}

// OK reports whether the step succeeded.
func (r StepResult) OK() bool { return r.ExitCode == 0 && r.Err == "" }

// RunStep executes one step, streaming redacted output into logFile.
//
// onPID receives the child's pid (which is also its process-group id) as soon
// as it starts, so a crash-recovery sweep can tell whether the tree is still
// alive after a restart.
func RunStep(ctx context.Context, spec StepSpec, logFile io.Writer, secrets []string, onPID func(int)) StepResult {
	res := StepResult{Name: spec.Name}
	start := time.Now()

	timeout := time.Duration(spec.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	grace := time.Duration(spec.KillGraceMs) * time.Millisecond

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	capped := newCapWriter(logFile, StepLogHeadBytes, StepLogTailBytes)
	red := NewRedactor(capped, secrets)
	finishLog := func() {
		// Order matters: flush the redactor's held partial first, then the cap's
		// retained tail, or the tail would be written before the bytes that
		// precede it.
		_ = red.Close()
		_ = capped.Close()
		res.LogBytes = capped.Written()
		res.LogTrunc = capped.Truncated()
	}

	// Deliberately exec.Command, not exec.CommandContext: CommandContext's
	// default cancel kills only cmd.Process, which leaves the rest of the
	// process group alive. Cancellation is owned by the watcher below so the
	// whole group is signalled.
	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	setProcessGroup(cmd)

	// os.Pipe rather than an io.Writer: exec hands the fd to the child
	// directly, so cmd.Wait does not block on an internal copy goroutine that a
	// surviving grandchild could keep open indefinitely.
	pr, pw, err := os.Pipe()
	if err != nil {
		res.Err = "pipe: " + err.Error()
		res.ExitCode = -1
		res.Elapsed = time.Since(start)
		finishLog()
		return res
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		res.Err = err.Error()
		res.ExitCode = -1
		res.Elapsed = time.Since(start)
		finishLog()
		return res
	}
	// Drop the parent's write end, or the reader never sees EOF.
	_ = pw.Close()

	pid := cmd.Process.Pid
	if onPID != nil {
		onPID(pid)
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = io.Copy(red, pr)
	}()

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-stepCtx.Done():
			killProcessGroupHard(pid, grace)
		case <-waitDone:
		}
	}()

	waitErr := cmd.Wait()
	close(waitDone)

	// The child is gone, but a grandchild may still hold the pipe's write end.
	// Give the reader a moment to drain naturally, then force it closed — and
	// join it either way. Skipping the join on the timeout branch would leave a
	// goroutine writing into the log after it is closed.
	select {
	case <-readerDone:
	case <-time.After(readerGrace):
		_ = pr.Close()
	}
	<-readerDone
	_ = pr.Close()
	finishLog()

	res.Elapsed = time.Since(start)

	// Context state first: a killed child reports a signal exit that says
	// nothing about *why* it was killed.
	switch {
	case errors.Is(stepCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = ExitTimeout
		res.Err = "timed out after " + timeout.String()
		return res
	case stepCtx.Err() != nil:
		res.Cancelled = true
		res.ExitCode = ExitCancelled
		res.Err = "cancelled"
		return res
	}

	if waitErr != nil {
		if ee, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = waitErr.Error()
		}
		return res
	}
	res.ExitCode = 0
	return res
}

// StepDir resolves a service's working directory inside a checkout, refusing
// anything that escapes it.
//
// The manifest is validated at parse time, but this is the last point before a
// path becomes a real cwd, and a symlink inside the checkout could still point
// outward — so the check is repeated against the resolved path.
func StepDir(checkout, serviceDir string) (string, error) {
	if err := validateDir(serviceDir); err != nil {
		return "", err
	}
	base, err := filepath.Abs(checkout)
	if err != nil {
		return "", err
	}
	dir := base
	if serviceDir != "" && serviceDir != "." {
		dir = filepath.Join(base, serviceDir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// A dir that does not exist yet is the manifest's problem to report at
		// run time, not a containment failure.
		if os.IsNotExist(err) {
			return dir, nil
		}
		return "", err
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	if resolved != resolvedBase && !strings.HasPrefix(resolved, resolvedBase+string(os.PathSeparator)) {
		return "", errors.New("service dir resolves outside the checkout")
	}
	return resolved, nil
}
