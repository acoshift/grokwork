package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grok-discord/internal/gitworktree"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

// idleCleanupInterval is how often the background sweeper runs.
const idleCleanupInterval = 24 * time.Hour

var idleCleanupOnce sync.Once

func (b *Bot) startIdleWorktreeCleanup() {
	idleCleanupOnce.Do(func() {
		ttl := b.cfg.WorktreeIdleTTL()
		log.Printf("bg: starting idle-worktree sweeper interval=%s ttl=%s initial_delay=30s",
			idleCleanupInterval, ttl)
		go b.runIdleWorktreeCleanup()
	})
}

func (b *Bot) runIdleWorktreeCleanup() {
	log.Printf("bg: idle-worktree sweeper running (waiting 30s before first sweep)")
	// Brief delay so gateway ready / first messages aren't competing with a sweep.
	time.Sleep(30 * time.Second)
	b.runIdleSweepCycle("initial")

	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		b.runIdleSweepCycle("tick")
	}
}

func (b *Bot) runIdleSweepCycle(reason string) {
	ttl := b.cfg.WorktreeIdleTTL()
	log.Printf("bg: idle-worktree sweep start reason=%s ttl=%s", reason, ttl)
	start := time.Now()
	n := b.sweepIdleWorktrees()
	log.Printf("bg: idle-worktree sweep done reason=%s removed=%d elapsed=%s",
		reason, n, time.Since(start).Round(time.Millisecond))
}

// sweepIdleWorktrees applies the configured TTL (0 disables).
func (b *Bot) sweepIdleWorktrees() int {
	ttl := b.cfg.WorktreeIdleTTL()
	if ttl <= 0 {
		log.Printf("bg: idle-worktree sweep skipped (ttl disabled)")
		return 0
	}
	return b.pruneIdleWorktrees(time.Now(), ttl)
}

// WorktreeInfo is a per-thread worktree row for the admin UI.
type WorktreeInfo struct {
	ThreadID     string    `json:"threadId"`
	Project      string    `json:"project"`
	Branch       string    `json:"branch"`
	Path         string    `json:"path"`
	LastActive   time.Time `json:"-"`
	LastActiveAt string    `json:"lastActiveAt,omitempty"`
	IdleFor      string    `json:"idleFor,omitempty"`
	Busy         bool      `json:"busy"`
	OnDisk       bool      `json:"onDisk"`
	HasSession   bool      `json:"hasSession"`
	IdlePastTTL  bool      `json:"idlePastTTL"`
}

type idleCandidate struct {
	threadID   string
	project    string
	path       string
	branch     string
	mainCwd    string
	last       time.Time
	onDisk     bool
	hasSession bool
}

// ListWorktrees returns all known thread worktrees (on disk and/or session-backed).
func (b *Bot) ListWorktrees() []WorktreeInfo {
	now := time.Now()
	ttl := b.cfg.WorktreeIdleTTL()
	cutoff := time.Time{}
	if ttl > 0 {
		cutoff = now.Add(-ttl)
	}

	all := b.collectAllWorktrees()
	out := make([]WorktreeInfo, 0, len(all))
	for _, c := range all {
		info := WorktreeInfo{
			ThreadID:   c.threadID,
			Project:    c.project,
			Branch:     c.branch,
			Path:       c.path,
			LastActive: c.last,
			Busy:       b.isThreadBusy(c.threadID),
			OnDisk:     c.onDisk,
			HasSession: c.hasSession,
		}
		if !c.last.IsZero() {
			info.LastActiveAt = c.last.UTC().Format(time.RFC3339)
			info.IdleFor = formatIdleFor(now.Sub(c.last))
			if !cutoff.IsZero() && !c.last.After(cutoff) {
				info.IdlePastTTL = true
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b WorktreeInfo) int {
		// Oldest first; empty last active last.
		switch {
		case a.LastActive.Equal(b.LastActive):
			if a.ThreadID < b.ThreadID {
				return -1
			}
			if a.ThreadID > b.ThreadID {
				return 1
			}
			return 0
		case a.LastActive.IsZero():
			return 1
		case b.LastActive.IsZero():
			return -1
		case a.LastActive.Before(b.LastActive):
			return -1
		default:
			return 1
		}
	})
	return out
}

// PruneWorktree removes one thread worktree (path + managed branch + session).
// Busy threads are refused.
func (b *Bot) PruneWorktree(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("thread id is required")
	}
	if b.isThreadBusy(threadID) {
		return fmt.Errorf("thread %s is busy (run or queue active)", threadID)
	}

	var found *idleCandidate
	for _, c := range b.collectAllWorktrees() {
		if c.threadID == threadID {
			cc := c
			found = &cc
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no worktree for thread %s", threadID)
	}
	return b.removeWorktreeCandidate(*found, "manual")
}

// PruneIdleNow removes worktrees past the configured idle TTL.
// Returns how many were removed. Errors when TTL cleanup is disabled.
func (b *Bot) PruneIdleNow() (int, error) {
	ttl := b.cfg.WorktreeIdleTTL()
	if ttl <= 0 {
		return 0, fmt.Errorf("idle cleanup is disabled (worktreeIdleTTLDays=0)")
	}
	n := b.pruneIdleWorktrees(time.Now(), ttl)
	return n, nil
}

// pruneIdleWorktrees removes per-thread worktrees (and their sessions/branches)
// that have been inactive for at least ttl. Returns how many were removed.
func (b *Bot) pruneIdleWorktrees(now time.Time, ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, c := range b.collectAllWorktrees() {
		if c.last.IsZero() || c.last.After(cutoff) {
			continue
		}
		if b.isThreadBusy(c.threadID) {
			log.Printf("idle-worktree: skip busy thread=%s", c.threadID)
			continue
		}
		if err := b.removeWorktreeCandidate(c, "idle"); err != nil {
			log.Printf("warn: idle-worktree remove thread=%s: %v", c.threadID, err)
			// removeWorktreeCandidate still drops session when possible
			continue
		}
		removed++
	}
	return removed
}

func (b *Bot) removeWorktreeCandidate(c idleCandidate, reason string) error {
	if c.mainCwd == "" {
		// Still drop session if present so the UI can clear the row.
		if c.hasSession {
			_ = b.sessions.Delete(c.threadID)
		}
		return fmt.Errorf("no main repo path for project %q", c.project)
	}

	path := c.path
	if path != "" {
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			path = ""
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	err := gitworktree.Remove(ctx, c.mainCwd, path, c.branch)
	cancel()
	if err != nil {
		log.Printf("warn: worktree remove (%s) thread=%s path=%s: %v", reason, c.threadID, path, err)
	} else {
		log.Printf("worktree: removed (%s) thread=%s project=%s branch=%s last=%s",
			reason, c.threadID, c.project, c.branch, formatLast(c.last))
	}
	if delErr := b.sessions.Delete(c.threadID); delErr != nil {
		log.Printf("warn: worktree session delete thread=%s: %v", c.threadID, delErr)
		if err == nil {
			err = delErr
		}
	}
	return err
}

func (b *Bot) collectAllWorktrees() []idleCandidate {
	byThread := map[string]idleCandidate{}

	onDisk, err := gitworktree.ListOnDisk(b.cfg.DataDir)
	if err != nil {
		log.Printf("warn: worktree list: %v", err)
	}
	for _, d := range onDisk {
		mainCwd, project := b.resolveProjectRepo(d.Project, "")
		c := idleCandidate{
			threadID: d.ThreadID,
			project:  project,
			path:     d.Path,
			branch:   gitworktree.BranchNameForUnit(d.ThreadID),
			mainCwd:  mainCwd,
			last:     gitworktree.DirModTime(d.Path),
			onDisk:   true,
		}
		if c.project == "" {
			c.project = d.Project
		}
		byThread[d.ThreadID] = c
	}

	for _, listed := range b.sessions.List() {
		e := listed.Entry
		threadID := listed.ThreadID
		if !sessionHasWorktree(e) {
			continue
		}

		last := parseRFC3339(e.UpdatedAt)
		path := e.Cwd
		if path == "" || path == e.MainCwd {
			path = gitworktree.WorktreePath(b.cfg.DataDir, e.Project, threadID)
		}
		branch := e.WorktreeBranch
		if branch == "" {
			branch = gitworktree.BranchNameForUnit(threadID)
		}
		mainCwd := e.MainCwd
		if mainCwd == "" {
			mainCwd, _ = b.resolveProjectRepo(e.Project, "")
		}

		existing, ok := byThread[threadID]
		if !ok {
			onDisk := false
			if path != "" {
				if st, err := os.Stat(path); err == nil && st.IsDir() {
					onDisk = true
				}
			}
			byThread[threadID] = idleCandidate{
				threadID:   threadID,
				project:    e.Project,
				path:       path,
				branch:     branch,
				mainCwd:    mainCwd,
				last:       last,
				onDisk:     onDisk,
				hasSession: true,
			}
			continue
		}
		existing.hasSession = true
		if e.Project != "" {
			existing.project = e.Project
		}
		if path != "" {
			existing.path = path
		}
		if branch != "" {
			existing.branch = branch
		}
		if mainCwd != "" {
			existing.mainCwd = mainCwd
		}
		if !last.IsZero() {
			existing.last = last
		}
		byThread[threadID] = existing
	}

	out := make([]idleCandidate, 0, len(byThread))
	for _, c := range byThread {
		if c.path != "" {
			if st, err := os.Stat(c.path); err != nil || !st.IsDir() {
				c.path = ""
				c.onDisk = false
			} else {
				c.onDisk = true
			}
		}
		if c.path == "" && c.branch == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func sessionHasWorktree(e sessionstore.Entry) bool {
	if e.WorktreeBranch != "" {
		return true
	}
	if e.Cwd != "" && e.MainCwd != "" && e.Cwd != e.MainCwd {
		return true
	}
	return false
}

func (b *Bot) resolveProjectRepo(project, mainCwd string) (repo, name string) {
	if mainCwd != "" {
		return mainCwd, project
	}
	if project == "" {
		return "", ""
	}
	if p, ok := b.cfg.ProjectPath(project); ok {
		return p, project
	}
	// On-disk segment may be sanitized; match against config names.
	for _, n := range b.cfg.ProjectNames() {
		if gitworktree.WorktreePath(b.cfg.DataDir, n, "x") == gitworktree.WorktreePath(b.cfg.DataDir, project, "x") {
			if p, ok := b.cfg.ProjectPath(n); ok {
				return p, n
			}
		}
	}
	return "", project
}

func (b *Bot) isThreadBusy(threadID string) bool {
	v, ok := b.states.Load(threadID)
	if !ok {
		return false
	}
	st, _ := v.(*threadState)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.job != nil || len(st.queue) > 0
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatLast(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatIdleFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
