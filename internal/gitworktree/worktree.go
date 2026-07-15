// Package gitworktree isolates concurrent Grok runs via per-thread git worktrees.
package gitworktree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Tree struct {
	Path   string
	Branch string
	Repo   string
}

// BranchPrefix is required for bot-managed branches (only these may be deleted).
const BranchPrefix = "grok/discord/"

func BranchName(threadID string) string {
	return BranchPrefix + threadID
}

func IsManagedBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	if !strings.HasPrefix(branch, BranchPrefix) {
		return false
	}
	rest := branch[len(BranchPrefix):]
	if rest == "" || rest == "." || rest == ".." {
		return false
	}
	if strings.Contains(rest, "..") || strings.HasPrefix(rest, "/") {
		return false
	}
	return true
}

func WorktreePath(dataDir, project, threadID string) string {
	return filepath.Join(dataDir, "worktrees", sanitizePathSegment(project), sanitizePathSegment(threadID))
}

func IsRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// CleanupIfPRDone removes the worktree/branch when the PR is merged or closed.
// Missing gh → cleaned=false with err set.
func CleanupIfPRDone(ctx context.Context, repo, dataDir, project, threadID string) (cleaned bool, state string, err error) {
	if repo == "" || threadID == "" {
		return false, "", nil
	}
	if !IsRepo(repo) {
		return false, "", nil
	}

	branch := BranchName(threadID)
	path := WorktreePath(dataDir, project, threadID)

	hasPath := false
	if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
		hasPath = true
	}
	hasBranch := branchExists(ctx, repo, branch)
	if !hasPath && !hasBranch {
		return false, "", nil
	}

	done, state, err := branchPRTerminal(ctx, repo, branch)
	if err != nil {
		return false, "", err
	}
	if !done {
		return false, "", nil
	}

	log.Printf("gitworktree: PR %s for branch %s — removing worktree path=%s", state, branch, path)
	if rmErr := Remove(ctx, repo, path, branch); rmErr != nil {
		return false, state, rmErr
	}
	if delErr := deleteRemoteBranch(ctx, repo, branch); delErr != nil {
		// Best-effort: remote may already be gone.
		log.Printf("gitworktree: remote branch delete %s: %v", branch, delErr)
	}
	return true, state, nil
}

func Ensure(ctx context.Context, repo, dataDir, project, threadID string) (Tree, error) {
	if repo == "" || threadID == "" {
		return Tree{}, fmt.Errorf("repo and threadID are required")
	}
	if !IsRepo(repo) {
		return Tree{}, fmt.Errorf("not a git repository: %s", repo)
	}

	branch := BranchName(threadID)
	path := WorktreePath(dataDir, project, threadID)
	t := Tree{Path: path, Branch: branch, Repo: repo}

	if ok, err := isUsableWorktree(ctx, repo, path); err != nil {
		return Tree{}, err
	} else if ok {
		log.Printf("gitworktree: reuse path=%s branch=%s", path, branch)
		return t, nil
	}

	if _, err := os.Stat(path); err == nil {
		log.Printf("gitworktree: removing unusable path %s", path)
		_ = Remove(ctx, repo, path, branch)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Tree{}, fmt.Errorf("mkdir worktree parent: %w", err)
	}

	err := runGit(ctx, repo, "worktree", "add", "-b", branch, path, "HEAD")
	if err != nil {
		if branchExists(ctx, repo, branch) {
			err = runGit(ctx, repo, "worktree", "add", path, branch)
		}
		if err != nil {
			return Tree{}, fmt.Errorf("git worktree add: %w", err)
		}
	}

	log.Printf("gitworktree: created path=%s branch=%s repo=%s", path, branch, repo)
	return t, nil
}

func Remove(ctx context.Context, repo, path, branch string) error {
	var errs []string

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := runGit(ctx, repo, "worktree", "remove", "--force", path); err != nil {
				_ = runGit(ctx, repo, "worktree", "prune")
				if rmErr := os.RemoveAll(path); rmErr != nil {
					errs = append(errs, fmt.Sprintf("remove path: %v (git: %v)", rmErr, err))
				} else {
					_ = runGit(ctx, repo, "worktree", "prune")
				}
			}
		}
	}

	if branch != "" && repo != "" {
		if !IsManagedBranch(branch) {
			errs = append(errs, fmt.Sprintf("refuse to delete unprotected branch %q (want prefix %s)", branch, BranchPrefix))
		} else if branchExists(ctx, repo, branch) {
			if err := runGit(ctx, repo, "branch", "-D", branch); err != nil {
				errs = append(errs, fmt.Sprintf("delete branch %s: %v", branch, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func isUsableWorktree(ctx context.Context, repo, path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !st.IsDir() {
		return false, nil
	}
	if !IsRepo(path) {
		return false, nil
	}
	mainCommon, err := gitOutput(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, err
	}
	wtCommon, err := gitOutput(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, nil
	}
	mainAbs, err := absGitPath(repo, mainCommon)
	if err != nil {
		return false, err
	}
	wtAbs, err := absGitPath(path, wtCommon)
	if err != nil {
		return false, nil
	}
	return mainAbs == wtAbs, nil
}

func absGitPath(base, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty git path")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Abs(filepath.Join(base, p))
}

func branchExists(ctx context.Context, repo, branch string) bool {
	err := runGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func branchPRTerminal(ctx context.Context, repo, branch string) (done bool, state string, err error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--head", branch,
		"--state", "all",
		"--json", "state",
		"--limit", "20",
	)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return false, "", fmt.Errorf("gh pr list: %s", msg)
	}

	var prs []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return false, "", fmt.Errorf("gh pr list json: %w", err)
	}
	states := make([]string, 0, len(prs))
	for _, pr := range prs {
		states = append(states, pr.State)
	}
	done, state = terminalPRState(states)
	return done, state, nil
}

func terminalPRState(states []string) (done bool, state string) {
	hasOpen := false
	var terminal string
	for _, s := range states {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "OPEN":
			hasOpen = true
		case "MERGED":
			terminal = "MERGED"
		case "CLOSED":
			if terminal == "" {
				terminal = "CLOSED"
			}
		}
	}
	if hasOpen || terminal == "" {
		return false, ""
	}
	return true, terminal
}

func deleteRemoteBranch(ctx context.Context, repo, branch string) error {
	if !IsManagedBranch(branch) {
		return fmt.Errorf("refuse to delete unprotected remote branch %q (want prefix %s)", branch, BranchPrefix)
	}
	out, err := gitOutput(ctx, repo, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return runGit(ctx, repo, "push", "origin", "--delete", branch)
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func sanitizePathSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "." || out == ".." || out == "" {
		return "_unknown"
	}
	return out
}
