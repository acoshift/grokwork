package gitworktree

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// AdoptOpts checks out an existing (unmanaged) PR head into the unit worktree.
// Unlike EnsureWith, this never creates a managed grokwork/ grok/web/ branch
// and never deletes the adopted branch.
type AdoptOpts struct {
	Branch           string
	PullNumber       int    // optional; fetch origin pull/N/head when origin/Branch is missing
	PreferredPrimary string // refused as a checkout target (plus main/master)
}

// NormalizeAdoptBranch strips refs/heads/ and surrounding space.
func NormalizeAdoptBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	branch, _ = strings.CutPrefix(branch, "refs/heads/")
	return strings.TrimSpace(branch)
}

// CanAdoptBranch reports whether branch is a safe PR-head checkout target.
// Managed prefixes, the configured primary, and main/master are refused so a
// session cannot land on a protected line by adopting a GitHub headRefName.
func CanAdoptBranch(branch, preferredPrimary string) bool {
	branch = NormalizeAdoptBranch(branch)
	if branch == "" || IsManagedBranch(branch) {
		return false
	}
	if preferredPrimary != "" && strings.EqualFold(branch, strings.TrimSpace(preferredPrimary)) {
		return false
	}
	switch strings.ToLower(branch) {
	case "main", "master", "head":
		return false
	}
	return validAdoptSyntax(branch)
}

func validAdoptSyntax(branch string) bool {
	if branch == "." || branch == ".." || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") {
		return false
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return false
	}
	for _, r := range branch {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validGitRefName(ctx context.Context, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--normalize", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// EnsureAdopted creates or reuses a worktree checked out to an existing branch
// (the GitHub PR head). The branch is never created from primary and is never
// deleted by this function.
func EnsureAdopted(ctx context.Context, repo, worktreesRoot, project, unitID string, opts AdoptOpts) (Tree, error) {
	if repo == "" || unitID == "" {
		return Tree{}, fmt.Errorf("repo and unitID are required")
	}
	if !IsRepo(repo) {
		return Tree{}, fmt.Errorf("not a git repository: %s", repo)
	}
	branch := NormalizeAdoptBranch(opts.Branch)
	if !CanAdoptBranch(branch, opts.PreferredPrimary) {
		return Tree{}, fmt.Errorf("refuse to adopt branch %q", branch)
	}
	if !validGitRefName(ctx, branch) {
		return Tree{}, fmt.Errorf("invalid git branch name %q", branch)
	}

	path := WorktreePath(worktreesRoot, project, unitID)
	t := Tree{Path: path, Branch: branch, Repo: repo}

	if ok, err := isUsableWorktree(ctx, repo, path); err != nil {
		return Tree{}, err
	} else if ok {
		if cur := headBranch(ctx, path); cur == branch {
			log.Printf("gitworktree: reuse adopted path=%s branch=%s", path, branch)
			return t, nil
		}
		log.Printf("gitworktree: replacing adopted path=%s (HEAD %q, want %q)", path, headBranch(ctx, path), branch)
		if err := removeWorktreeAtPath(ctx, repo, path); err != nil {
			return Tree{}, err
		}
	} else if _, err := os.Stat(path); err == nil {
		if err := removeWorktreeAtPath(ctx, repo, path); err != nil {
			return Tree{}, err
		}
	} else {
		_ = pruneStaleWorktrees(ctx, repo)
	}

	if other := adoptCheckoutConflict(ctx, repo, path, branch); other != "" {
		return Tree{}, fmt.Errorf("branch %q is already checked out at %s", branch, other)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Tree{}, fmt.Errorf("mkdir worktree parent: %w", err)
	}

	start, err := resolveAdoptStart(ctx, repo, branch, opts.PullNumber)
	if err != nil {
		return Tree{}, err
	}

	if branchExists(ctx, repo, branch) {
		err = runGit(ctx, repo, "worktree", "add", path, branch)
	} else {
		err = runGit(ctx, repo, "worktree", "add", "--track", "-b", branch, path, start)
		if err != nil {
			// FETCH_HEAD / a raw SHA has no upstream to track.
			err = runGit(ctx, repo, "worktree", "add", "-b", branch, path, start)
		}
	}
	if err != nil {
		return Tree{}, fmt.Errorf("git worktree add: %w", err)
	}
	log.Printf("gitworktree: adopted path=%s branch=%s repo=%s", path, branch, repo)
	return t, nil
}

func resolveAdoptStart(ctx context.Context, repo, branch string, pullNumber int) (string, error) {
	if branchExists(ctx, repo, branch) {
		return branch, nil
	}
	remote := "origin/" + branch
	if !commitRefExists(ctx, repo, remote) {
		_ = runGit(ctx, repo, "fetch", "--no-tags", "origin", branch+":refs/remotes/origin/"+branch)
	}
	if commitRefExists(ctx, repo, remote) {
		return remote, nil
	}
	if pullNumber > 0 {
		if err := FetchPullHead(ctx, repo, pullNumber); err != nil {
			log.Printf("gitworktree: fetch pull/%d/head: %v", pullNumber, err)
		} else if commitRefExists(ctx, repo, "FETCH_HEAD") {
			return "FETCH_HEAD", nil
		}
	}
	return "", fmt.Errorf("PR branch %q not found locally or on origin", branch)
}

// adoptCheckoutConflict returns the path of another worktree (including the
// main checkout) that already has branch, or empty if the unit path is free
// to take it. Never steals a human checkout.
func adoptCheckoutConflict(ctx context.Context, repo, wantPath, branch string) string {
	if headBranch(ctx, repo) == branch {
		return repo
	}
	list, err := listLinkedWorktrees(ctx, repo)
	if err != nil {
		return ""
	}
	wantAbs := absPathBestEffort(wantPath)
	for _, wt := range list {
		if wt.Branch != branch {
			continue
		}
		if absPathBestEffort(wt.Path) == wantAbs {
			continue
		}
		return wt.Path
	}
	return ""
}

func absPathBestEffort(p string) string {
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
