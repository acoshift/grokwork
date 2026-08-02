package bot

import (
	"strings"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// FindByPR returns candidate units for project + GitHub PR (bound PRs[] only).
// Terminal labels excluded unless includeTerminal. Order matches FindByIssue.
// Agentic PR-review units (SessionKindPRReview) are always excluded — they bind
// the PR for the detail Sessions list only, never for Address reuse.
func (b *Bot) FindByPR(project, owner, repo string, number int, includeTerminal bool) []IssueSessionHit {
	return b.findPRHits(project, owner, repo, number, includeTerminal, false)
}

// FindPRSessions returns every unit that binds the PR, including agent-review
// units and terminal labels. Used by the PR detail Sessions section and the
// "Go to session" head link.
func (b *Bot) FindPRSessions(project, owner, repo string, number int) []IssueSessionHit {
	return b.findPRHits(project, owner, repo, number, true, true)
}

func (b *Bot) findPRHits(project, owner, repo string, number int, includeTerminal, includeReview bool) []IssueSessionHit {
	if b == nil || b.sessions == nil || number <= 0 {
		return nil
	}
	target := sessionstore.TrackedPR{
		Owner:  strings.TrimSpace(owner),
		Repo:   strings.TrimSpace(repo),
		Number: number,
	}
	target.FillOwnerRepoFromURL()
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	list := b.sessions.List()
	var hits []IssueSessionHit
	for _, listed := range list {
		if !strings.EqualFold(listed.Project, project) {
			continue
		}
		if !entryBindsPR(listed.Entry, target) {
			continue
		}
		if !includeReview && listed.IsPRReview() {
			continue
		}
		if !includeTerminal && sessionstore.IsTerminalLabel(listed.EffectiveLabel()) {
			continue
		}
		qlen := b.queueLen(listed.ThreadID)
		busy := false
		if _, ok := b.getJob(listed.ThreadID); ok {
			busy = true
		} else if qlen > 0 {
			busy = true
		}
		hits = append(hits, IssueSessionHit{
			ThreadID:    listed.ThreadID,
			Project:     listed.Project,
			Goal:        listed.Goal,
			Label:       listed.EffectiveLabel(),
			OwnerName:   listed.OwnerName,
			OwnerID:     listed.OwnerID,
			UpdatedAt:   listed.UpdatedAt,
			Busy:        busy,
			QueueLen:    qlen,
			HasWorktree: strings.TrimSpace(listed.WorktreeBranch) != "" || strings.TrimSpace(listed.Cwd) != "",
			DiscordURL:  listed.DiscordURL,
			SessionKind: listed.SessionKind,
		})
	}
	sortIssueHits(hits)
	return hits
}

func entryBindsPR(e sessionstore.Entry, target sessionstore.TrackedPR) bool {
	e.NormalizePRs()
	for _, pr := range e.PRs {
		if sessionstore.SamePR(pr, target) {
			return true
		}
		// Also match by stable PRKey when both have URL/owner.
		if target.PRKey() != "" && pr.PRKey() == target.PRKey() {
			return true
		}
	}
	return false
}
