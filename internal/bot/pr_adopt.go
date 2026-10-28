package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
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
		// Git will not check the branch out twice, and this path must not
		// move it or register the checkout as a worktree this unit may delete.
		// The main working tree stays the project cwd (a nested repo's first
		// turn already ran there). A checkout outside the worktree root — a
		// human or IDE worktree — is the directory the run uses. Another
		// unit's tree under the worktree root still fails: two runs cannot
		// share it, and idle cleanup may remove it.
		if gitworktree.CheckedOutAtMain(err, repo) {
			cwd := proj.Cwd
			if cwd == "" {
				cwd = repo
			}
			log.Printf("task: PR branch %s already checked out at %s — continuing in %s thread=%s", branch, repo, cwd, threadID)
			return cwd, "", true, nil
		}
		if busy, ok := errors.AsType[*gitworktree.ErrBranchCheckedOut](err); ok && busy != nil && strings.TrimSpace(busy.Path) != "" {
			if pathWithin(b.worktreesRoot(), busy.Path) || !gitworktree.IsRepo(busy.Path) {
				return "", "", true, err
			}
			log.Printf("task: PR branch %s already checked out at %s — continuing there thread=%s", branch, busy.Path, threadID)
			return busy.Path, "", true, nil
		}
		return "", "", true, err
	}
	b.stampAdoptedWorktree(threadID, tree.Path, tree.Branch, repo)
	return tree.Path, tree.Branch, true, nil
}

func (b *Bot) worktreesRoot() string {
	if b == nil || b.cfg == nil {
		return ""
	}
	return b.cfg.WorktreesRoot()
}

// borrowedPRCheckout is a directory the run may use but must not record as
// this unit's worktree. Idle and terminal cleanup remove Entry.Cwd when it
// differs from the project directory, which would delete a human checkout.
func (b *Bot) borrowedPRCheckout(proj projectRef, threadID, cwd, wtBranch string) bool {
	cwd = strings.TrimSpace(cwd)
	if wtBranch != "" || cwd == "" || samePath(cwd, proj.Cwd) {
		return false
	}
	if b == nil || b.cfg == nil {
		return true
	}
	if samePath(cwd, gitworktree.PRAskPath(b.cfg.DataDir, proj.Name, threadID)) {
		return false
	}
	if samePath(cwd, gitworktree.WorktreePath(b.cfg.WorktreesRoot(), proj.Name, threadID)) {
		return false
	}
	return true
}

// storedRunCwd is the cwd written onto the session. A borrowed checkout is
// replaced so a later sweep cannot remove it. A previously owned cwd is kept.
func (b *Bot) storedRunCwd(proj projectRef, threadID, runCwd, wtBranch, prevCwd string) string {
	if !b.borrowedPRCheckout(proj, threadID, runCwd, wtBranch) {
		return runCwd
	}
	if prevCwd != "" && !b.borrowedPRCheckout(proj, threadID, prevCwd, "") {
		return prevCwd
	}
	return proj.Cwd
}

func borrowedCheckoutNote() string {
	return "\nThe working directory is an existing checkout of this session's pull-request branch. Stay in this directory. Do not create another worktree, do not switch branches, and do not remove this checkout. Commit only the changes for this task.\n"
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

func samePath(a, b string) bool {
	a = absPath(a)
	b = absPath(b)
	return a != "" && a == b
}

func pathWithin(root, path string) bool {
	root = absPath(root)
	path = absPath(path)
	if root == "" || path == "" || root == path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func absPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
