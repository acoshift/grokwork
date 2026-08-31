package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// idleCleanupInterval is how often the background sweeper runs.
const idleCleanupInterval = 24 * time.Hour

func (b *Bot) startIdleWorktreeCleanup() {
	if b == nil {
		return
	}
	b.idleCleanupOnce.Do(func() {
		ttl := b.cfg.WorktreeIdleTTL()
		termTTL := b.cfg.TerminalSessionTTL()
		log.Printf("bg: starting idle-worktree sweeper interval=%s worktreeTTL=%s terminalSessionTTL=%s initial_delay=30s",
			idleCleanupInterval, ttl, termTTL)
		go b.runIdleWorktreeCleanup()
	})
}

func (b *Bot) runIdleWorktreeCleanup() {
	ctx := b.bgContext()
	log.Printf("bg: idle-worktree sweeper running (waiting 30s before first sweep)")
	// Brief delay so gateway ready / first messages aren't competing with a sweep.
	if !sleepCtx(ctx, 30*time.Second) {
		log.Printf("bg: idle-worktree sweeper stopped before first sweep")
		return
	}
	b.runIdleSweepCycle("initial")

	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("bg: idle-worktree sweeper stopped")
			return
		case <-ticker.C:
			b.runIdleSweepCycle("tick")
		}
	}
}

func (b *Bot) runIdleSweepCycle(reason string) {
	ttl := b.cfg.WorktreeIdleTTL()
	termTTL := b.cfg.TerminalSessionTTL()
	log.Printf("bg: idle sweep start reason=%s worktreeTTL=%s terminalSessionTTL=%s", reason, ttl, termTTL)
	start := time.Now()
	nWT := b.sweepIdleWorktrees()
	nSess := b.sweepTerminalSessions()
	nAsk := b.sweepPRAskUnits()
	nCP := gitworktree.SweepExpiredCherryPicks(b.bgContext(), b.cfg.DataDir)
	log.Printf("bg: idle sweep done reason=%s worktrees=%d terminalSessions=%d prAsks=%d cherrypickJobs=%d elapsed=%s",
		reason, nWT, nSess, nAsk, nCP, time.Since(start).Round(time.Millisecond))
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

// sweepTerminalSessions deletes done/abandoned session tombstones older than
// TerminalSessionTTL (based on UpdatedAt). 0 / unset disables.
func (b *Bot) sweepTerminalSessions() int {
	ttl := b.cfg.TerminalSessionTTL()
	if ttl <= 0 {
		log.Printf("bg: terminal-session sweep skipped (ttl disabled)")
		return 0
	}
	return b.pruneTerminalSessions(time.Now(), ttl)
}

// pruneTerminalSessions removes store entries whose effective label is done or
// abandoned and whose UpdatedAt is at least ttl in the past. Busy threads are
// skipped. Leftover worktrees are removed best-effort before Delete.
func (b *Bot) pruneTerminalSessions(now time.Time, ttl time.Duration) int {
	if b == nil || b.sessions == nil || ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, listed := range b.sessions.List() {
		e := listed.Entry
		if !sessionstore.IsTerminalLabel(e.EffectiveLabel()) {
			continue
		}
		// Open cases keep their store row until /close even if mislabeled.
		if e.IsCase() && !e.IsCaseClosed() {
			continue
		}
		last := parseRFC3339(e.UpdatedAt)
		if last.IsZero() || last.After(cutoff) {
			continue
		}
		threadID := listed.ThreadID
		if b.isThreadBusy(threadID) {
			log.Printf("terminal-session: skip busy thread=%s", threadID)
			continue
		}
		// Best-effort worktree cleanup before dropping the row.
		mainCwd := e.MainCwd
		if mainCwd == "" {
			mainCwd, _ = b.resolveProjectRepo(e.Project, "")
		}
		branch := e.WorktreeBranch
		if branch == "" {
			branch = gitworktree.BranchNameForUnit(threadID)
		}
		path, _ := gitworktree.ResolveSessionWorktreePath(b.cfg.WorktreesRoot(), e.Project, threadID, e.Cwd, mainCwd)
		if mainCwd != "" && (path != "" || branch != "") {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if rmErr := gitworktree.Remove(ctx, mainCwd, path, branch); rmErr != nil {
				log.Printf("warn: terminal-session worktree remove thread=%s: %v", threadID, rmErr)
			}
			cancel()
		}
		if err := b.sessions.Delete(threadID); err != nil {
			log.Printf("warn: terminal-session delete thread=%s: %v", threadID, err)
			continue
		}
		log.Printf("terminal-session: deleted thread=%s label=%s last=%s", threadID, e.EffectiveLabel(), e.UpdatedAt)
		removed++
	}
	return removed
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
			info.IdleFor = formatCoarseDuration(now.Sub(c.last))
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

// PruneWorktree removes one thread worktree (path + managed branch).
// Ephemeral sessions become abandoned tombstones; terminal/PR/case sessions keep metadata.
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

// pruneIdleWorktrees removes per-thread worktrees (and managed branches) that
// have been inactive for at least ttl. Ephemeral sessions become abandoned
// tombstones; terminal/PR/case sessions keep metadata. Returns how many removed.
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
			// removeWorktreeCandidate still finalizes the session (keep or abandon)
			continue
		}
		removed++
	}
	return removed
}

func (b *Bot) removeWorktreeCandidate(c idleCandidate, reason string) error {
	// After worktree removal: cases / PR-linked / already-terminal units keep their
	// existing label and metadata (clear worktree fields only). Everything else
	// becomes an abandoned tombstone — never hard-delete the session row so the
	// sessions list still shows final state (same shape as resetThreadCore).
	preserveMeta := false
	if c.hasSession && b.sessions != nil {
		if e, ok := b.sessions.Get(c.threadID); ok {
			e.NormalizePRs()
			if e.IsCase() || e.HasAnyPR() || sessionstore.IsTerminalLabel(e.EffectiveLabel()) {
				preserveMeta = true
			}
		}
	}

	if c.mainCwd == "" {
		if c.hasSession {
			if patchErr := b.finalizeSessionAfterWorktreeGone(c.threadID, "", preserveMeta); patchErr != nil {
				log.Printf("warn: session finalize after worktree gone thread=%s: %v", c.threadID, patchErr)
			}
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
	if c.hasSession {
		if patchErr := b.finalizeSessionAfterWorktreeGone(c.threadID, c.mainCwd, preserveMeta); patchErr != nil {
			log.Printf("warn: session finalize after worktree gone thread=%s: %v", c.threadID, patchErr)
			if err == nil {
				err = patchErr
			}
		} else if preserveMeta {
			log.Printf("idle-worktree: session kept thread=%s reason=%s", c.threadID, reason)
		} else {
			log.Printf("idle-worktree: session abandoned thread=%s reason=%s", c.threadID, reason)
		}
	}
	return err
}

// finalizeSessionAfterWorktreeGone clears worktree fields. When preserveMeta is
// false (ephemeral unit with no PRs/case/terminal label), also drops the Grok
// session id and sets label abandoned so the list shows a tombstone instead of
// deleting the entry.
func (b *Bot) finalizeSessionAfterWorktreeGone(threadID, mainCwd string, preserveMeta bool) error {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return nil
	}
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Cwd = ""
		ent.WorktreeBranch = ""
		if ent.MainCwd == "" && mainCwd != "" {
			ent.MainCwd = mainCwd
		}
		if preserveMeta {
			return
		}
		ent.SessionID = ""
		_ = ent.SetLabelManual(sessionstore.LabelAbandoned)
		// Terminal lifecycle starts the TTL / Active recency clock.
		ent.StampTurn(ent.LastUser)
	})
	return err
}

func (b *Bot) collectAllWorktrees() []idleCandidate {
	byThread := map[string]idleCandidate{}

	wtRoot := b.cfg.WorktreesRoot()
	onDisk, err := gitworktree.ListOnDisk(wtRoot)
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

	if b.sessions != nil {
		for _, listed := range b.sessions.List() {
			e := listed.Entry
			threadID := listed.ThreadID
			if listed.IsPRAsk() {
				continue
			}
			if !sessionHasWorktree(e) {
				continue
			}

			last := parseRFC3339(e.UpdatedAt)
			mainCwd := e.MainCwd
			if mainCwd == "" {
				mainCwd, _ = b.resolveProjectRepo(e.Project, "")
			}
			// Prefer live dirs: session cwd if still present, else canonical worktrees root
			// (covers dataDir renames like grok-discord → grokwork).
			path, pathOnDisk := gitworktree.ResolveSessionWorktreePath(
				wtRoot, e.Project, threadID, e.Cwd, mainCwd,
			)
			if pathOnDisk && e.Cwd != "" && e.Cwd != path {
				// Heal stale absolute cwd left after a dataDir / host path rename.
				b.healSessionWorktreeCwd(threadID, path)
			}
			branch := e.WorktreeBranch
			if branch == "" {
				branch = gitworktree.BranchNameForUnit(threadID)
			}

			existing, ok := byThread[threadID]
			if !ok {
				byThread[threadID] = idleCandidate{
					threadID:   threadID,
					project:    e.Project,
					path:       path,
					branch:     branch,
					mainCwd:    mainCwd,
					last:       last,
					onDisk:     pathOnDisk,
					hasSession: true,
				}
				continue
			}
			existing.hasSession = true
			if e.Project != "" {
				existing.project = e.Project
			}
			// Never replace a verified on-disk path with a stale session cwd.
			if pathOnDisk {
				existing.path = path
				existing.onDisk = true
			} else if existing.path == "" {
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
	}

	out := make([]idleCandidate, 0, len(byThread))
	for _, c := range byThread {
		if c.path != "" {
			if st, err := os.Stat(c.path); err != nil || !st.IsDir() {
				// Last chance: re-resolve under current worktrees root (session may have been wrong).
				if c.project != "" && c.threadID != "" {
					alt, ok := gitworktree.ResolveSessionWorktreePath(wtRoot, c.project, c.threadID, "", c.mainCwd)
					if ok {
						c.path = alt
						c.onDisk = true
						if c.hasSession {
							b.healSessionWorktreeCwd(c.threadID, alt)
						}
					} else {
						c.path = ""
						c.onDisk = false
					}
				} else {
					c.path = ""
					c.onDisk = false
				}
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

// healSessionWorktreeCwd rewrites Entry.Cwd when the worktree moved (e.g. dataDir rename).
func (b *Bot) healSessionWorktreeCwd(threadID, newCwd string) {
	threadID = strings.TrimSpace(threadID)
	newCwd = strings.TrimSpace(newCwd)
	if b == nil || b.sessions == nil || threadID == "" || newCwd == "" {
		return
	}
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if ent.Cwd == newCwd {
			return
		}
		log.Printf("session: heal worktree cwd thread=%s old=%q new=%q", threadID, ent.Cwd, newCwd)
		ent.Cwd = newCwd
	})
	if err != nil {
		log.Printf("warn: heal session cwd thread=%s: %v", threadID, err)
	}
}

// sweepPRAskUnits deletes throwaway PR-ask units that are idle past the
// worktree TTL, or whose PR is already MERGED/CLOSED on a sibling tracked
// session. No tombstone: leftover history would reappear on /sessions.
func (b *Bot) sweepPRAskUnits() int {
	if b == nil || b.sessions == nil {
		return 0
	}
	ttl := b.cfg.WorktreeIdleTTL()
	now := time.Now()
	removed := 0
	for _, listed := range b.sessions.List() {
		if !listed.IsPRAsk() {
			continue
		}
		threadID := listed.ThreadID
		if b.isThreadBusy(threadID) {
			continue
		}
		e := listed.Entry
		idle := false
		if ttl > 0 {
			last := parseRFC3339(e.UpdatedAt)
			if !last.IsZero() && now.Sub(last) >= ttl {
				idle = true
			}
		}
		terminal := b.askPRTrackedTerminal(e.Project, e.AskPRKey)
		if !idle && !terminal {
			continue
		}
		if err := b.deletePRAskUnit(threadID, e); err != nil {
			log.Printf("warn: pr-ask sweep thread=%s: %v", threadID, err)
			continue
		}
		log.Printf("pr-ask: deleted thread=%s idle=%v terminal=%v last=%s", threadID, idle, terminal, e.UpdatedAt)
		removed++
	}
	return removed
}

func (b *Bot) askPRTrackedTerminal(project, askKey string) bool {
	project = strings.TrimSpace(project)
	askKey = strings.ToLower(strings.TrimSpace(askKey))
	if project == "" || askKey == "" || b.sessions == nil {
		return false
	}
	for _, listed := range b.sessions.List() {
		if !strings.EqualFold(listed.Project, project) {
			continue
		}
		if listed.IsPRAsk() {
			continue
		}
		e := listed.Entry
		e.NormalizePRs()
		for _, pr := range e.PRs {
			if sessionstore.FormatAskPRKey(pr.Owner, pr.Repo, pr.Number) != askKey {
				continue
			}
			st := strings.ToUpper(strings.TrimSpace(pr.State))
			if st == "MERGED" || st == "CLOSED" {
				return true
			}
		}
	}
	return false
}

func (b *Bot) deletePRAskUnit(threadID string, e sessionstore.Entry) error {
	mainCwd := e.MainCwd
	if mainCwd == "" {
		mainCwd, _ = b.resolveProjectRepo(e.Project, "")
	}
	path := strings.TrimSpace(e.Cwd)
	if path == "" && b.cfg != nil {
		path = gitworktree.PRAskPath(b.cfg.DataDir, e.Project, threadID)
	}
	if mainCwd != "" && path != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if rmErr := gitworktree.Remove(ctx, mainCwd, path, ""); rmErr != nil {
			log.Printf("warn: pr-ask checkout remove thread=%s: %v", threadID, rmErr)
		}
		cancel()
	}
	if b.history != nil {
		if err := b.history.Delete(threadID); err != nil {
			log.Printf("warn: pr-ask history delete thread=%s: %v", threadID, err)
		}
	}
	return b.sessions.Delete(threadID)
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
	root := b.cfg.WorktreesRoot()
	for _, n := range b.cfg.ProjectNames() {
		if gitworktree.WorktreePath(root, n, "x") == gitworktree.WorktreePath(root, project, "x") {
			if p, ok := b.cfg.ProjectPath(n); ok {
				return p, n
			}
		}
	}
	return "", project
}

func (b *Bot) isThreadBusy(threadID string) bool {
	v, ok := b.states.Load(threadID)
	if ok {
		st, _ := v.(*threadState)
		if st != nil {
			st.mu.Lock()
			busy := st.job != nil || len(st.queue) > 0
			st.mu.Unlock()
			if busy {
				return true
			}
		}
	}
	if b.resumeEnabled() && b.runs != nil && b.runs.HasWork(threadID) {
		return true
	}
	return false
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

// formatCoarseDuration is chip-sized duration text ("<1m", "45m", "3h", "2d").
// Shared by the worktrees board's idle column and the case SLA badges so the
// two do not phrase the same span two ways.
func formatCoarseDuration(d time.Duration) string {
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
