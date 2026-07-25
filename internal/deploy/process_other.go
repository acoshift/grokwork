//go:build !unix

package deploy

import (
	"os"
	"os/exec"
	"time"
)

// DefaultKillGrace mirrors the unix constant.
const DefaultKillGrace = 5 * time.Second

// Non-unix stubs. Deploys are not supported on these platforms in any
// meaningful way (no process groups means no reliable tree kill), but the
// package must still build.

func setProcessGroup(*exec.Cmd) {}

func killProcessGroupHard(pid int, _ time.Duration) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func processGroupAlive(int) bool { return false }

func pidAlive(int) bool { return false }
