package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// CancelRun cancels the active run for a unit (Discord-free export of
// cancelCurrentRun). ok is false when the thread is idle; queued follow-ups
// survive and auto-promote.
func (b *Bot) CancelRun(threadID, who string) (string, bool) {
	if b == nil {
		return "No run in progress for this thread.", false
	}
	return b.cancelCurrentRun(threadID, who)
}

// ResetUnit clears the session, worktree, managed branch, and queue for a unit
// (Discord-free export of resetThreadCore). It refuses while a run is busy: on
// refusal err is non-nil and msg explains why. msg is always set.
func (b *Bot) ResetUnit(threadID string) (string, error) {
	if b == nil {
		return "", fmt.Errorf("bot is nil")
	}
	return b.resetThreadCore(threadID)
}

// SetSessionLabel sets the lifecycle label for a unit (Discord-free core of
// /label). "auto" clears the manual override. Manual sets are the documented
// escape hatch for closed cases and are allowed here, mirroring Discord's
// handleLabel (which never refuses a manual /label); the K18 auto-label freeze
// still lives in sessionstore.ApplyAutoLabel/SuggestAutoLabel, so ClearLabelManual
// on a closed case is a no-op on the stored label. Best-effort brief-card refresh
// only for Discord units when the gateway is connected; never surfaces Discord
// errors to the caller.
func (b *Bot) SetSessionLabel(threadID, label string) error {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return fmt.Errorf("no session store")
	}
	label = strings.TrimSpace(label)
	auto := strings.EqualFold(label, "auto")
	var lab string
	if !auto {
		parsed, ok := sessionstore.ParseLabel(label)
		if !ok {
			return fmt.Errorf("unknown label %q", label)
		}
		lab = parsed
	}
	var setErr error
	_, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if auto {
			ent.ClearLabelManual()
			return
		}
		setErr = ent.SetLabelManual(lab)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no session for thread %s", threadID)
	}
	if setErr != nil {
		return setErr
	}
	b.maybeRefreshBriefWeb(threadID)
	return nil
}

// SetSessionGoal sets the sticky goal for a unit (Discord-free core of
// /brief goal). The goal is clamped exactly as the Discord path clamps it.
// Best-effort brief refresh follows the same rule as SetSessionLabel.
func (b *Bot) SetSessionGoal(threadID, goal string) error {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return fmt.Errorf("no session store")
	}
	goal = clampGoal(goal)
	if goal == "" {
		return fmt.Errorf("goal text is empty")
	}
	_, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Goal = goal
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no session for thread %s", threadID)
	}
	b.maybeRefreshBriefWeb(threadID)
	return nil
}

// ClaimThread performs a full ownership takeover of a unit (Discord-free core of
// /claim): the previous primary owner becomes the sole co-owner, and an ownership
// shell Entry is created when no session exists yet.
func (b *Bot) ClaimThread(threadID string, actor Actor) error {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return fmt.Errorf("no session store")
	}
	if actor.ID == "" {
		return fmt.Errorf("claim requires an identity")
	}
	_, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		if ent.IsOwner(actor.ID) {
			return
		}
		prevID := ent.OwnerID
		// Full takeover: reset co-owners, then keep only the previous primary so
		// the list does not grow unbounded across repeated claims.
		ent.CoOwnerIDs = nil
		ent.SetOwner(actor.ID, actor.String())
		if prevID != "" && prevID != actor.ID {
			ent.AddCoOwner(prevID)
		}
	})
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// No session yet: create an ownership shell so cancel/reset can bind later.
	e := sessionstore.Entry{}
	e.SetOwner(actor.ID, actor.String())
	return b.sessions.Set(threadID, e)
}

// maybeRefreshBriefWeb best-effort refreshes the pinned brief card after a web
// label/goal write. Clean no-op for web-native (w_*) units and when the gateway
// is down; Discord errors are logged, never returned to the caller.
func (b *Bot) maybeRefreshBriefWeb(threadID string) {
	if b == nil || strings.TrimSpace(threadID) == "" || gitworktree.IsWebUnitID(threadID) {
		return
	}
	s := b.Discord()
	if s == nil || b.sessions == nil {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || e.BriefMsgID == "" {
		return
	}
	if _, err := b.refreshBriefCard(s, threadID, ""); err != nil {
		log.Printf("web: brief refresh thread=%s: %v", threadID, err)
	}
}
