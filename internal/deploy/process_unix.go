//go:build unix

package deploy

import (
	"os/exec"
	"syscall"
	"time"
)

// Process helpers are owned here rather than reused from internal/grokrun.
//
// grokrun.KillProcessGroup sends a group SIGTERM, then polls the LEADER
// (syscall.Kill(pid, 0)) and returns the moment it exits — so its group SIGKILL
// is unreachable whenever the leader dies promptly, which is the normal case for
// `sh -c`. Grandchildren that honour SIGTERM still die there; one that ignores
// or traps it never receives the SIGKILL and survives the run. Build tools do
// trap TERM to clean up, and a deploy must not leave those behind.
//
// The liveness probe below has the same leader-versus-group problem in reverse
// and must target the group, so two of these three functions differ from
// grokrun's anyway; duplicating the one-line Setpgid is less coupling than
// importing the agent package for it.

// DefaultKillGrace is how long the group gets to exit on SIGTERM before SIGKILL.
const DefaultKillGrace = 5 * time.Second

// setProcessGroup makes the child a process-group leader so its whole tree can
// be signalled at once.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroupHard SIGTERMs the group, waits for the whole group to go away
// (not just its leader), then unconditionally SIGKILLs it.
func killProcessGroupHard(pgid int, grace time.Duration) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if grace <= 0 {
		grace = DefaultKillGrace
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pgid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Unconditional: reaching here means something in the group ignored SIGTERM.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// processGroupAlive reports whether any process remains in the group.
//
// Signal 0 to a negative pid probes the group, so this stays true while a
// grandchild outlives the `sh` that spawned it — exactly the orphan a restart
// must not race.
func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	return syscall.Kill(-pgid, 0) == nil
}

// pidAlive probes a single pid (tests use it to assert a grandchild is gone).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
