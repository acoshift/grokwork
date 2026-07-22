package gitworktree

import (
	"context"
	"fmt"
	"strings"
)

// DirectShipResult is the outcome of a fast-forward push of a managed session
// branch onto the project primary (No-PR / direct-to-primary mode).
type DirectShipResult struct {
	PrimaryBranch string
	FromSHA       string // origin/primary before push (best-effort)
	ToSHA         string // session HEAD that was pushed
	Noop          bool   // already up to date; no push needed
}

// ResolvePrimaryBranch returns the short primary branch name and origin remote
// ref (e.g. "main", "origin/main") for a checkout. Uses origin/HEAD, then common
// origin/* names, then the current local branch of the main checkout.
func ResolvePrimaryBranch(ctx context.Context, repo string) (name, remoteRef string, err error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || !IsRepo(repo) {
		return "", "", fmt.Errorf("not a git repository")
	}
	start := resolveNewBranchStart(ctx, repo)
	if start == "HEAD" {
		// Fall back to current branch name if possible.
		if cur, cerr := gitOutput(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD"); cerr == nil {
			cur = strings.TrimSpace(cur)
			if cur != "" && cur != "HEAD" {
				return cur, "origin/" + cur, nil
			}
		}
		return "", "", fmt.Errorf("could not resolve primary branch")
	}
	name = strings.TrimPrefix(start, "origin/")
	name = strings.TrimPrefix(name, "refs/remotes/origin/")
	if name == "" {
		return "", "", fmt.Errorf("empty primary branch name from %q", start)
	}
	if strings.HasPrefix(start, "origin/") {
		return name, start, nil
	}
	return name, "origin/" + name, nil
}

// DirectShipFF fast-forwards the project primary to the session worktree HEAD
// via `git push origin <sha>:refs/heads/<primary>` (never --force).
//
// Remote push rejection is the canonical non-fast-forward path. A local
// ancestor pre-check is used only for friendly errors and noop detection.
// sessionBranch must be a managed branch (IsManagedBranch).
//
// Clean tree means no staged/unstaged changes to *tracked* files; untracked
// scratch files do not block ship.
func DirectShipFF(ctx context.Context, mainRepo, worktreePath, sessionBranch, primary string) (DirectShipResult, error) {
	var out DirectShipResult
	mainRepo = strings.TrimSpace(mainRepo)
	worktreePath = strings.TrimSpace(worktreePath)
	sessionBranch = strings.TrimSpace(sessionBranch)
	primary = strings.TrimSpace(primary)

	if mainRepo == "" || !IsRepo(mainRepo) {
		return out, fmt.Errorf("main repo is not a git repository")
	}
	if worktreePath == "" || !IsRepo(worktreePath) {
		return out, fmt.Errorf("worktree is not a git repository")
	}
	if !IsManagedBranch(sessionBranch) {
		return out, fmt.Errorf("refuse to ship non-managed branch %q", sessionBranch)
	}
	if primary == "" {
		name, _, err := ResolvePrimaryBranch(ctx, mainRepo)
		if err != nil {
			return out, err
		}
		primary = name
	}
	out.PrimaryBranch = primary

	// Confirm worktree is on the expected managed branch (or at least the branch exists).
	headBranch, _ := gitOutput(ctx, worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	headBranch = strings.TrimSpace(headBranch)
	if headBranch != "" && headBranch != "HEAD" && headBranch != sessionBranch {
		return out, fmt.Errorf("worktree HEAD is %q, expected managed branch %q", headBranch, sessionBranch)
	}

	if dirty, err := hasTrackedDirt(ctx, worktreePath); err != nil {
		return out, err
	} else if dirty {
		return out, fmt.Errorf("worktree has uncommitted changes to tracked files")
	}

	sessionHead, err := gitOutput(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return out, fmt.Errorf("session HEAD: %w", err)
	}
	sessionHead = strings.TrimSpace(sessionHead)
	if sessionHead == "" {
		return out, fmt.Errorf("empty session HEAD")
	}
	out.ToSHA = sessionHead

	// Best-effort fetch for fresher origin/primary and local tracking refs.
	_ = runGit(ctx, mainRepo, "fetch", "origin", "--prune")

	remoteRef := "origin/" + primary
	fromSHA, fromErr := gitOutput(ctx, mainRepo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if fromErr == nil {
		out.FromSHA = strings.TrimSpace(fromSHA)
	}

	// Noop: session head already is origin/primary (or primary tip if no remote ref).
	if out.FromSHA != "" && out.FromSHA == sessionHead {
		out.Noop = true
		return out, nil
	}
	// Also noop if local primary matches and remote missing.
	if out.FromSHA == "" {
		if local, lerr := gitOutput(ctx, mainRepo, "rev-parse", "--verify", primary+"^{commit}"); lerr == nil {
			if strings.TrimSpace(local) == sessionHead {
				out.Noop = true
				out.FromSHA = sessionHead
				return out, nil
			}
		}
	}

	// Friendly non-ff pre-check (push still authoritative).
	if out.FromSHA != "" {
		if err := runGit(ctx, mainRepo, "merge-base", "--is-ancestor", out.FromSHA, sessionHead); err != nil {
			return out, fmt.Errorf("non-fast-forward: %s is not an ancestor of session HEAD (primary may have advanced)", remoteRef)
		}
	}

	// Objects live in the shared object store; push from main repo by SHA.
	dest := "refs/heads/" + primary
	if err := runGit(ctx, mainRepo, "push", "origin", sessionHead+":"+dest); err != nil {
		return out, fmt.Errorf("push to %s rejected (non-fast-forward or protected): %w", primary, err)
	}
	// Refresh tracking ref best-effort.
	_ = runGit(ctx, mainRepo, "fetch", "origin", primary+":"+remoteRef)
	NoteFetched(mainRepo)
	return out, nil
}

// hasTrackedDirt reports staged or unstaged changes to tracked files.
// Untracked files are ignored.
func hasTrackedDirt(ctx context.Context, dir string) (bool, error) {
	// Porcelain v1: XY path — skip lines that are untracked (??).
	out, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "??") {
			continue
		}
		// Any other status means tracked change (including renames, etc.).
		return true, nil
	}
	return false, nil
}
