package gitworktree

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultIdleTTL matches config.DefaultWorktreeIdleTTLDays (30 days).
// Runtime TTL comes from config.worktreeIdleTTLDays; this constant is for tests/docs.
const DefaultIdleTTL = 30 * 24 * time.Hour

// OnDisk is a worktree directory found under worktreesRoot/<project>/<threadID>.
// Project and ThreadID are the on-disk path segments (already sanitized).
type OnDisk struct {
	Project  string
	ThreadID string
	Path     string
}

// ListOnDisk returns every thread worktree directory under worktreesRoot.
// Missing root is not an error (returns nil).
func ListOnDisk(worktreesRoot string) ([]OnDisk, error) {
	root := worktreesRoot
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	projEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []OnDisk
	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		project := pe.Name()
		if project == "." || project == ".." {
			continue
		}
		projDir := filepath.Join(root, project)
		threadEntries, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, te := range threadEntries {
			if !te.IsDir() {
				continue
			}
			threadID := te.Name()
			if threadID == "." || threadID == ".." {
				continue
			}
			out = append(out, OnDisk{
				Project:  project,
				ThreadID: threadID,
				Path:     filepath.Join(projDir, threadID),
			})
		}
	}
	return out, nil
}

// DirModTime returns the modification time of path, or zero if unavailable.
func DirModTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
