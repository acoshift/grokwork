package runjournal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LockFile is data/runs/.lock content.
type LockFile struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Host      string `json:"host,omitempty"`
}

func (s *Store) lockPath() string {
	return filepath.Join(s.dir, ".lock")
}

// TryLock acquires the single-instance lock. If another live process holds it, returns error.
// Dead or recycled PIDs (command line not grokwork) are adopted.
func (s *Store) TryLock(pid int, startedAt time.Time, host string) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.lockPath())
	if err == nil {
		var prev LockFile
		if json.Unmarshal(raw, &prev) == nil && prev.PID > 0 && prev.PID != pid {
			if processAlive(prev.PID) && looksLikeGrokwork(prev.PID) {
				return fmt.Errorf("runjournal: another grokwork holds lock (pid %d host %s)", prev.PID, prev.Host)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	lf := LockFile{
		PID:       pid,
		StartedAt: startedAt.UTC().Format(time.RFC3339),
		Host:      host,
	}
	b, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.lockPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.lockPath())
}

// Unlock removes the lock file if held by this pid (best-effort).
func (s *Store) Unlock(pid int) error {
	if s == nil {
		return nil
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.lockPath())
	if err != nil {
		return nil
	}
	var lf LockFile
	if json.Unmarshal(raw, &lf) == nil && lf.PID != 0 && lf.PID != pid {
		return nil
	}
	_ = os.Remove(s.lockPath())
	return nil
}

// looksLikeGrokwork reports whether pid's command line mentions grokwork.
func looksLikeGrokwork(pid int) bool {
	cmd := processCommandLine(pid)
	if cmd == "" {
		// Ambiguous: if we cannot inspect, refuse (safer than stealing lock).
		return true
	}
	low := strings.ToLower(cmd)
	return strings.Contains(low, "grokwork")
}

// LooksLikeGrokCLI reports whether pid looks like a grok CLI child we started.
func LooksLikeGrokCLI(pid int, grokBin string) bool {
	cmd := processCommandLine(pid)
	if cmd == "" {
		return false
	}
	low := strings.ToLower(cmd)
	if strings.Contains(low, "grok") {
		return true
	}
	if grokBin != "" {
		base := strings.ToLower(filepath.Base(grokBin))
		if base != "" && strings.Contains(low, base) {
			return true
		}
	}
	return false
}

func processCommandLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
