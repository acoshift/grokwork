//go:build unix

package grokrun

import (
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcessGroup sends SIGTERM then SIGKILL to the process group of pid.
func KillProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	// Negative pid targets the process group (requires Setpgid).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// Also signal the leader in case group kill failed (no Setpgid).
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// ProcessAlive reports whether pid accepts signal 0.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
