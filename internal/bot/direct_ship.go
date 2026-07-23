package bot

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// ensureShipMode stamps Entry.ShipMode on first run from project config and
// returns the sticky mode for this thread ("pr" or "direct").
func (b *Bot) ensureShipMode(threadID, project string) string {
	if b == nil || b.sessions == nil || threadID == "" {
		return sessionstore.ShipModePR
	}
	wantDirect := b.cfg != nil && b.cfg.ProjectDirectToPrimary(project)
	var mode string
	_, ok, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		if e.Project == "" {
			e.Project = project
		}
		switch strings.TrimSpace(e.ShipMode) {
		case sessionstore.ShipModeDirect, sessionstore.ShipModePR:
			mode = e.ShipMode
			return
		}
		if wantDirect {
			e.ShipMode = sessionstore.ShipModeDirect
		} else {
			e.ShipMode = sessionstore.ShipModePR
		}
		mode = e.ShipMode
	})
	if err != nil {
		log.Printf("ship: stamp mode thread=%s: %v", threadID, err)
	}
	if !ok {
		// No session yet — create a minimal stamp (label path may also create).
		e := sessionstore.Entry{Project: project}
		if wantDirect {
			e.ShipMode = sessionstore.ShipModeDirect
		} else {
			e.ShipMode = sessionstore.ShipModePR
		}
		mode = e.ShipMode
		if err := b.sessions.Set(threadID, e); err != nil {
			log.Printf("ship: create mode thread=%s: %v", threadID, err)
		}
	}
	if mode == "" {
		if wantDirect {
			return sessionstore.ShipModeDirect
		}
		return sessionstore.ShipModePR
	}
	return mode
}

// shipLockFor returns the mutex for a main checkout path (abs + EvalSymlinks).
func (b *Bot) shipLockFor(mainRepo string) *sync.Mutex {
	key := shipLockKey(mainRepo)
	if key == "" {
		key = "_"
	}
	b.shipMu.Lock()
	defer b.shipMu.Unlock()
	if b.shipLocks == nil {
		b.shipLocks = map[string]*sync.Mutex{}
	}
	if m, ok := b.shipLocks[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	b.shipLocks[key] = m
	return m
}

func shipLockKey(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return filepath.Clean(repo)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// shipDirectAfterTask fast-forwards primary when the run gate passes.
// Returns true when ship succeeded (including noop). Does not remove the worktree
// (caller does after completion card when queue empty).
func (b *Bot) shipDirectAfterTask(s *discordgo.Session, present bool, threadID string, proj projectRef, runCwd, wtBranch string, result grokrun.Result) bool {
	if b == nil || threadID == "" || wtBranch == "" {
		return false
	}
	if result.Cancelled || result.Code != 0 || result.MaxTurnsReached {
		log.Printf("ship: skip thread=%s cancelled=%v code=%d maxTurns=%v",
			threadID, result.Cancelled, result.Code, result.MaxTurnsReached)
		if present && s != nil && result.MaxTurnsReached {
			if _, err := s.ChannelMessageSend(threadID,
				"Direct ship skipped (max turns reached — half-done work must not land on primary). Worktree kept."); err != nil {
				log.Printf("ship: notify skip: %v", err)
			}
		}
		return false
	}
	if !gitworktree.IsManagedBranch(wtBranch) {
		log.Printf("ship: skip non-managed branch=%s thread=%s", wtBranch, threadID)
		return false
	}
	mainRepo := strings.TrimSpace(proj.Cwd)
	if mainRepo == "" || !gitworktree.IsRepo(mainRepo) {
		log.Printf("ship: no main repo thread=%s", threadID)
		return false
	}
	worktree := strings.TrimSpace(runCwd)
	if worktree == "" {
		log.Printf("ship: empty worktree path thread=%s", threadID)
		return false
	}

	mu := b.shipLockFor(mainRepo)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := gitworktree.DirectShipFF(ctx, mainRepo, worktree, wtBranch, "")
	if err != nil {
		log.Printf("ship: failed thread=%s: %v", threadID, err)
		if present && s != nil {
			primary := res.PrimaryBranch
			if primary == "" {
				primary = "primary"
			}
			msg := fmt.Sprintf(
				"Could not ship to **%s** (%s).\n"+
					"Commits remain on `%s` in this thread's worktree.\n"+
					"Primary may have advanced (another session or human push).\n"+
					"Use `@Grok /reset` then re-run so a fresh worktree starts from the current primary tip.\n"+
					"(Web: Prune worktree, then re-run.)",
				primary, err.Error(), wtBranch,
			)
			if _, sendErr := s.ChannelMessageSend(threadID, msg); sendErr != nil {
				log.Printf("ship: notify fail: %v", sendErr)
			}
		}
		return false
	}

	now := time.Now().UTC().Format(time.RFC3339)
	short := res.ToSHA
	if len(short) > 12 {
		short = short[:12]
	}
	_, _, patchErr := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		e.ShipMode = sessionstore.ShipModeDirect
		e.ShippedSHA = res.ToSHA
		e.ShippedAt = now
		if res.PrimaryBranch != "" {
			e.PrimaryBranch = res.PrimaryBranch
		}
		e.ApplyAutoLabel(sessionstore.LabelDone)
	})
	if patchErr != nil {
		log.Printf("ship: session patch thread=%s: %v", threadID, patchErr)
	}

	if present && s != nil {
		var msg string
		if res.Noop {
			msg = fmt.Sprintf("Already on **%s** (`%s`) — nothing new to ship.", res.PrimaryBranch, short)
		} else {
			msg = fmt.Sprintf("Shipped to **%s** (`%s`) — no PR (direct-to-primary).", res.PrimaryBranch, short)
		}
		if _, sendErr := s.ChannelMessageSend(threadID, msg); sendErr != nil {
			log.Printf("ship: notify ok: %v", sendErr)
		}
	}
	log.Printf("ship: ok thread=%s primary=%s sha=%s noop=%v", threadID, res.PrimaryBranch, short, res.Noop)
	return true
}

// maybeRemoveDirectWorktree removes the managed worktree after a successful
// direct ship when the queue is empty. Keeps the session entry.
func (b *Bot) maybeRemoveDirectWorktree(threadID string, proj projectRef, runCwd, wtBranch string) {
	if b == nil || threadID == "" || wtBranch == "" {
		return
	}
	if b.queueLen(threadID) > 0 {
		return
	}
	mainRepo := strings.TrimSpace(proj.Cwd)
	if mainRepo == "" {
		return
	}
	path := runCwd
	if path == "" {
		path, _ = gitworktree.ResolveSessionWorktreePath(b.cfg.WorktreesRoot(), proj.Name, threadID, "", mainRepo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Direct ship success path: remove while job may still be held — allowed only
	// when queue is empty (no follow-up will reuse this dirty tree).
	if err := gitworktree.Remove(ctx, mainRepo, path, wtBranch); err != nil {
		log.Printf("ship: worktree remove thread=%s: %v", threadID, err)
		return
	}
	log.Printf("ship: removed worktree thread=%s branch=%s", threadID, wtBranch)
	// Clear worktree fields but keep session (ShipMode, SessionID, ownership, …).
	if _, _, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		e.Cwd = ""
		e.WorktreeBranch = ""
		// Keep MainCwd for recreate hints.
		if e.MainCwd == "" {
			e.MainCwd = mainRepo
		}
	}); err != nil {
		log.Printf("ship: clear worktree fields thread=%s: %v", threadID, err)
	}
}
