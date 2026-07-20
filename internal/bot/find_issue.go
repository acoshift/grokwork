package bot

import (
	"slices"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// IssueSessionHit is one work unit that already binds a given issue.
type IssueSessionHit struct {
	ThreadID    string
	Project     string
	Goal        string
	Label       string
	OwnerName   string
	OwnerID     string
	UpdatedAt   string
	Busy        bool
	QueueLen    int
	HasWorktree bool
	DiscordURL  string
}

// IsThreadBusy reports an active run or non-empty follow-up queue.
func (b *Bot) IsThreadBusy(threadID string) bool {
	if b == nil || threadID == "" {
		return false
	}
	if _, ok := b.getJob(threadID); ok {
		return true
	}
	return b.queueLen(threadID) > 0
}

// FindByIssue returns candidate units for project + GitHub issue (bound Issues[] only).
// Terminal labels (done/abandoned) are excluded unless includeTerminal is true.
// Order: busy/queued first, then open worktree, then newest UpdatedAt.
func (b *Bot) FindByIssue(project, owner, repo string, number int, includeTerminal bool) []IssueSessionHit {
	if b == nil || b.sessions == nil || number <= 0 {
		return nil
	}
	target := sessionstore.TrackedIssue{
		Owner:  strings.TrimSpace(owner),
		Repo:   strings.TrimSpace(repo),
		Number: number,
	}
	target.FillFromURL()
	return b.findIssueHits(project, target, includeTerminal)
}

// FindByLinearIssue returns candidate units for project + Linear identifier (case-insensitive).
func (b *Bot) FindByLinearIssue(project, identifier string, includeTerminal bool) []IssueSessionHit {
	if b == nil || b.sessions == nil {
		return nil
	}
	id := sessionstore.NormalizeLinearIdentifier(identifier)
	if id == "" {
		return nil
	}
	target := sessionstore.TrackedIssue{
		Provider:   sessionstore.ProviderLinear,
		Identifier: id,
	}
	return b.findIssueHits(project, target, includeTerminal)
}

func (b *Bot) findIssueHits(project string, target sessionstore.TrackedIssue, includeTerminal bool) []IssueSessionHit {
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
		if !entryBindsIssue(listed.Entry, target) {
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
		})
	}
	sortIssueHits(hits)
	return hits
}

func entryBindsIssue(e sessionstore.Entry, target sessionstore.TrackedIssue) bool {
	for _, iss := range e.Issues {
		if sessionstore.SameIssue(iss, target) {
			return true
		}
	}
	return false
}

func sortIssueHits(hits []IssueSessionHit) {
	slices.SortStableFunc(hits, func(a, b IssueSessionHit) int {
		if a.Busy != b.Busy {
			if a.Busy {
				return -1
			}
			return 1
		}
		if a.HasWorktree != b.HasWorktree {
			if a.HasWorktree {
				return -1
			}
			return 1
		}
		// Newest UpdatedAt first (RFC3339 lexicographic; empty last).
		switch {
		case a.UpdatedAt == b.UpdatedAt:
			return strings.Compare(a.ThreadID, b.ThreadID)
		case a.UpdatedAt == "":
			return 1
		case b.UpdatedAt == "":
			return -1
		case a.UpdatedAt > b.UpdatedAt:
			return -1
		case a.UpdatedAt < b.UpdatedAt:
			return 1
		default:
			return 0
		}
	})
}

// DiscordThreadURL builds a jump link when guild and thread are known.
func DiscordThreadURL(guildID, threadID string) string {
	guildID = strings.TrimSpace(guildID)
	threadID = strings.TrimSpace(threadID)
	if guildID == "" || threadID == "" {
		return ""
	}
	// Web-native units are not Discord channel snowflakes.
	if gitworktree.IsWebUnitID(threadID) {
		return ""
	}
	return "https://discord.com/channels/" + guildID + "/" + threadID
}

// ParseRFC3339OrZero parses UpdatedAt for tests/helpers.
func ParseRFC3339OrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}
