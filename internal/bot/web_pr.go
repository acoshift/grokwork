package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// ApplyPRTerminalState finds every session tracking owner/repo#number, sets State
// (MERGED or CLOSED), and runs terminal cleanup when that thread's PRs are all done.
// Returns affected thread IDs. Discord cards are not required (s may be nil).
func (b *Bot) ApplyPRTerminalState(owner, repo string, number int, state string) []string {
	if b == nil || b.sessions == nil || number <= 0 {
		return nil
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	state = strings.ToUpper(strings.TrimSpace(state))
	if state != "MERGED" && state != "CLOSED" {
		return nil
	}
	var affected []string
	for _, listed := range b.sessions.List() {
		threadID := listed.ThreadID
		e, ok := b.sessions.Get(threadID)
		if !ok {
			continue
		}
		e.NormalizePRs()
		matched := false
		for _, pr := range e.PRs {
			if prMatches(pr, owner, repo, number) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		key := ""
		for _, pr := range e.PRs {
			if prMatches(pr, owner, repo, number) {
				key = pr.PRKey()
				break
			}
		}
		if key == "" {
			continue
		}
		_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
			ok := ent.PatchPR(key, func(p *sessionstore.TrackedPR) {
				p.State = state
				p.IsDraft = false
			})
			if ok {
				ent.ApplyAutoLabel(ent.SuggestAutoLabel(false))
			}
		})
		if err != nil {
			log.Printf("web-pr: patch thread=%s: %v", threadID, err)
			continue
		}
		affected = append(affected, threadID)
		b.tryCleanupTerminalPR(threadID)
	}
	return affected
}

func prMatches(pr sessionstore.TrackedPR, owner, repo string, number int) bool {
	pr.FillOwnerRepoFromURL()
	if pr.Number != number {
		return false
	}
	if owner != "" && repo != "" {
		return strings.EqualFold(pr.Owner, owner) && strings.EqualFold(pr.Repo, repo)
	}
	return true
}

// FindThreadsByPR returns thread IDs tracking the PR (read-only helper for tests/UI).
func (b *Bot) FindThreadsByPR(owner, repo string, number int) []string {
	if b == nil || b.sessions == nil {
		return nil
	}
	var out []string
	for _, listed := range b.sessions.List() {
		e, ok := b.sessions.Get(listed.ThreadID)
		if !ok {
			continue
		}
		e.NormalizePRs()
		for _, pr := range e.PRs {
			if prMatches(pr, owner, repo, number) {
				out = append(out, listed.ThreadID)
				break
			}
		}
	}
	return out
}

// WebPRSelector builds a URL selector for logging/display.
func WebPRSelector(owner, repo string, number int) string {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner != "" && repo != "" && number > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
	}
	if number > 0 {
		return fmt.Sprintf("#%d", number)
	}
	return ""
}
