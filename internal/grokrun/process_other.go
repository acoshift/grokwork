//go:build !unix

package grokrun

import (
	"os"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {}

// KillProcessGroup kills the process if possible (no process groups on this OS).
func KillProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// ProcessAlive always returns false on non-unix (cannot probe reliably).
func ProcessAlive(pid int) bool {
	_ = pid
	return false
}
