package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// allPRsMerged reports that every tracked PR is MERGED (not merely terminal).
// A feature item whose PR was closed unmerged is NOT done.
func allPRsMerged(e sessionstore.Entry) bool {
	e.NormalizePRs()
	if len(e.PRs) == 0 {
		return false
	}
	for _, p := range e.PRs {
		// Same normalization as sessionstore.isTerminalState — a lowercase
		// "merged" is terminal there and must be merged here too.
		if strings.ToUpper(strings.TrimSpace(p.State)) != "MERGED" {
			return false
		}
	}
	return true
}

// syncEpicChecklist checks the feature-issue tasklist boxes for a session whose
// PRs all merged. Deterministic follow-on of an already-gated merge: the bot
// only flips "[ ]" to "[x]" on lines annotated with this session's link.
//
// Issue-body edits are last-write-wins; a human edit between fetch and write can
// clobber, but each edit stays a single-line flip so the collision surface is tiny.
func (b *Bot) syncEpicChecklist(threadID string, e sessionstore.Entry) {
	if b == nil || threadID == "" {
		return
	}
	e.NormalizePRs()
	// Require AllPRsTerminal + every PR MERGED (closed-unmerged is not done).
	if !e.AllPRsTerminal() || !allPRsMerged(e) {
		return
	}
	// Deployment kill-switch for host-credential writes. Per-user capability is
	// not consulted: there is no acting user in a poller.
	if b.cfg == nil || !b.cfg.FeatureGitHubWrites() {
		return
	}

	repoDir := b.prViewCwd(e)
	run := b.ghRun()
	for _, iss := range e.Issues {
		if iss.IsLinear() {
			continue
		}
		prov := strings.ToLower(strings.TrimSpace(iss.Provider))
		if prov != "" && prov != "github" {
			continue
		}
		owner := strings.TrimSpace(iss.Owner)
		repo := strings.TrimSpace(iss.Repo)
		if iss.Number <= 0 || owner == "" || repo == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		info, err := ghpr.ViewIssueWith(ctx, run, repoDir, iss.Number, owner, repo)
		if err != nil {
			cancel()
			log.Printf("epic checklist: view %s/%s#%d thread=%s: %v", owner, repo, iss.Number, threadID, err)
			continue
		}
		body2, changed := CheckTasklistLine(info.Body, threadID)
		if !changed {
			cancel()
			continue
		}
		editErr := ghpr.EditIssueBodyWith(ctx, run, repoDir, owner, repo, iss.Number, body2)
		cancel()
		if editErr != nil {
			log.Printf("epic checklist: edit %s/%s#%d thread=%s: %v", owner, repo, iss.Number, threadID, editErr)
		}
		// Audit only when an edit was attempted (success or failure).
		b.auditChecklistCheck(threadID, e.Project, owner, repo, iss.Number, editErr)
	}
}

func (b *Bot) auditChecklistCheck(threadID, project, owner, repo string, number int, err error) {
	if b == nil || b.audit == nil {
		return
	}
	detail := map[string]any{
		"project":  project,
		"owner":    owner,
		"repo":     repo,
		"number":   number,
		"threadId": threadID,
	}
	// No acting user in the poller path.
	b.auditCmd(audit.ActionIssueChecklistCheck, Actor{}, threadID, project, err, detail)
}
