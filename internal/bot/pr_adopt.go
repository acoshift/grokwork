package bot

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// trackedPRToAdopt returns the single open PR whose head this session should
// keep working on, instead of minting a grokwork-managed branch.
//
// Used for imported-PR shells (no worktree yet) and for later turns after
// SessionKindImportedPR is cleared — WorktreeBranch is the adopted head.
func trackedPRToAdopt(e sessionstore.Entry, primary string) (pr sessionstore.TrackedPR, branch string, ok bool) {
	if e.IsPRReview() || e.IsPRAsk() || e.IsDirectShip() {
		return sessionstore.TrackedPR{}, "", false
	}
	if gitworktree.IsManagedBranch(e.WorktreeBranch) {
		return sessionstore.TrackedPR{}, "", false
	}
	open := e.OpenPRs()
	if len(open) != 1 {
		return sessionstore.TrackedPR{}, "", false
	}
	pr = open[0]
	branch = strings.TrimSpace(e.WorktreeBranch)
	if branch == "" {
		branch = gitworktree.NormalizeAdoptBranch(pr.HeadRef)
	}
	if !gitworktree.CanAdoptBranch(branch, primary) {
		return pr, "", false
	}
	return pr, branch, true
}

func (b *Bot) ensureAdoptedPRWorktree(ctx context.Context, proj projectRef, threadID string, e sessionstore.Entry, primary string) (cwd, branch string, ok bool, err error) {
	pr, branch, ok := trackedPRToAdopt(e, primary)
	if !ok {
		return "", "", false, nil
	}
	repo := proj.Cwd
	if pr.Owner != "" && pr.Repo != "" {
		if local, rerr := gitworktree.ResolveLocalRepo(ctx, proj.Cwd, pr.Owner, pr.Repo); rerr == nil && gitworktree.IsRepo(local) {
			repo = local
		}
	}
	if !gitworktree.IsRepo(repo) {
		return "", "", false, nil
	}
	tree, err := gitworktree.EnsureAdopted(ctx, repo, b.cfg.WorktreesRoot(), proj.Name, threadID, gitworktree.AdoptOpts{
		Branch:           branch,
		PullNumber:       pr.Number,
		PreferredPrimary: primary,
	})
	if err != nil {
		return "", "", true, err
	}
	b.stampAdoptedWorktree(threadID, tree.Path, tree.Branch, repo)
	return tree.Path, tree.Branch, true, nil
}

func (b *Bot) stampAdoptedWorktree(threadID, path, branch, mainCwd string) {
	if b == nil || b.sessions == nil || threadID == "" {
		return
	}
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Cwd = path
		ent.WorktreeBranch = branch
		if ent.MainCwd == "" {
			ent.MainCwd = mainCwd
		}
	})
	if err != nil {
		log.Printf("warn: stamp adopted worktree thread=%s: %v", threadID, err)
	}
}

// existingPRForPrompt is the PR a ship run should update rather than recreate.
func existingPRForPrompt(e sessionstore.Entry) (sessionstore.TrackedPR, bool) {
	if e.IsPRReview() || e.IsPRAsk() {
		return sessionstore.TrackedPR{}, false
	}
	open := e.OpenPRs()
	if len(open) != 1 {
		return sessionstore.TrackedPR{}, false
	}
	return open[0], true
}

func existingPRPromptLabel(pr sessionstore.TrackedPR) string {
	pr.FillOwnerRepoFromURL()
	sel := ""
	if pr.Owner != "" && pr.Repo != "" && pr.Number > 0 {
		sel = fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	} else if pr.Number > 0 {
		sel = fmt.Sprintf("#%d", pr.Number)
	}
	if u := strings.TrimSpace(pr.URL); u != "" {
		if sel != "" && !strings.EqualFold(sel, u) {
			return sel + " (" + u + ")"
		}
		if sel == "" {
			return u
		}
	}
	return sel
}
