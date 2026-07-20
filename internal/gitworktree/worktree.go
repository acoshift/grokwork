// Package gitworktree isolates concurrent Grok runs via per-thread git worktrees.
package gitworktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Managed branch prefixes. Only branches under these prefixes may be deleted by the bot.
const (
	DiscordBranchPrefix = "grok/discord/"
	WebBranchPrefix     = "grok/web/"
	// BranchPrefix is the Discord default; kept for call-site compatibility.
	BranchPrefix = DiscordBranchPrefix
)

// EnsureOpts selects which managed branch prefix Ensure/Cleanup use.
// Empty BranchPrefix means DiscordBranchPrefix.
type EnsureOpts struct {
	BranchPrefix string
}

// BranchName returns the Discord-managed branch for a unit id (thread snowflake).
func BranchName(unitID string) string {
	return DiscordBranchPrefix + unitID
}

// BranchNameWithPrefix returns prefix+unitID after normalizing the prefix.
func BranchNameWithPrefix(prefix, unitID string) string {
	return NormalizePrefix(prefix) + unitID
}

// NormalizePrefix returns a known managed prefix; empty or unknown → Discord.
func NormalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	switch prefix {
	case WebBranchPrefix, strings.TrimSuffix(WebBranchPrefix, "/"):
		return WebBranchPrefix
	case DiscordBranchPrefix, strings.TrimSuffix(DiscordBranchPrefix, "/"), "":
		return DiscordBranchPrefix
	default:
		// Allow exact known values only; anything else falls back to Discord.
		if strings.HasPrefix(prefix, "grok/web") {
			return WebBranchPrefix
		}
		return DiscordBranchPrefix
	}
}

// PrefixForUnitID chooses branch prefix from unit id form (w_* → web).
func PrefixForUnitID(unitID string) string {
	if IsWebUnitID(unitID) {
		return WebBranchPrefix
	}
	return DiscordBranchPrefix
}

// PrefixFromBranch returns the managed prefix for a branch, or empty if unmanaged.
func PrefixFromBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	for _, p := range []string{WebBranchPrefix, DiscordBranchPrefix} {
		if strings.HasPrefix(branch, p) {
			rest := branch[len(p):]
			if rest != "" && rest != "." && rest != ".." && !strings.Contains(rest, "..") && !strings.HasPrefix(rest, "/") {
				return p
			}
		}
	}
	return ""
}

// IsWebUnitID reports design form w_<suffix> work unit ids (not Discord snowflakes).
func IsWebUnitID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "w_") || len(id) < 4 {
		return false
	}
	rest := id[2:]
	for _, r := range rest {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// NewWebUnitID allocates a unique web-native unit id (w_ + 32 hex chars).
func NewWebUnitID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("gitworktree: crypto/rand: " + err.Error())
	}
	return "w_" + hex.EncodeToString(b[:])
}

// BranchNameForUnit picks Discord or web prefix from the unit id form.
func BranchNameForUnit(unitID string) string {
	return BranchNameWithPrefix(PrefixForUnitID(unitID), unitID)
}

func IsManagedBranch(branch string) bool {
	return PrefixFromBranch(branch) != ""
}

func WorktreePath(dataDir, project, unitID string) string {
	return filepath.Join(dataDir, "worktrees", sanitizePathSegment(project), sanitizePathSegment(unitID))
}

// ResolveSessionWorktreePath picks the best on-disk worktree for a unit.
// Prefer sessionCwd when it still exists; otherwise the canonical path under
// dataDir. This heals sessions that stored absolute paths under an old data
// directory (e.g. …/grok-discord/data/worktrees/… after a rename to grokwork).
// When nothing is on disk, returns the canonical path with onDisk=false.
func ResolveSessionWorktreePath(dataDir, project, unitID, sessionCwd, mainCwd string) (path string, onDisk bool) {
	canonical := WorktreePath(dataDir, project, unitID)
	sessionCwd = strings.TrimSpace(sessionCwd)
	mainCwd = strings.TrimSpace(mainCwd)

	dirOK := func(p string) bool {
		if p == "" {
			return false
		}
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	}

	// Live session path wins when it is a real worktree dir (not main checkout).
	if sessionCwd != "" && sessionCwd != mainCwd && dirOK(sessionCwd) {
		return sessionCwd, true
	}
	if dirOK(canonical) {
		return canonical, true
	}
	return canonical, false
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
// Uses Discord branch naming for unitID (backward compatible).
// Missing gh → cleaned=false with err set.
func CleanupIfPRDone(ctx context.Context, repo, dataDir, project, unitID string) (cleaned bool, state string, err error) {
	return CleanupIfPRDoneWith(ctx, repo, dataDir, project, unitID, EnsureOpts{})
}

// CleanupIfPRDoneWith is like CleanupIfPRDone but honors BranchPrefix (or empty → Discord).
// If branch is already known, pass EnsureOpts{BranchPrefix: PrefixFromBranch(branch)} or use
// the unit id form so PrefixForUnitID applies when BranchPrefix is empty and unit is w_*.
func CleanupIfPRDoneWith(ctx context.Context, repo, dataDir, project, unitID string, opts EnsureOpts) (cleaned bool, state string, err error) {
	if repo == "" || unitID == "" {
		return false, "", nil
	}
	if !IsRepo(repo) {
		return false, "", nil
	}

	prefix := opts.BranchPrefix
	if strings.TrimSpace(prefix) == "" {
		prefix = PrefixForUnitID(unitID)
	} else {
		prefix = NormalizePrefix(prefix)
	}
	branch := prefix + unitID
	path := WorktreePath(dataDir, project, unitID)

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

// Ensure creates or reuses a Discord-prefix worktree for unitID.
func Ensure(ctx context.Context, repo, dataDir, project, unitID string) (Tree, error) {
	return EnsureWith(ctx, repo, dataDir, project, unitID, EnsureOpts{})
}

// EnsureWith creates or reuses a worktree; BranchPrefix empty uses PrefixForUnitID(unitID).
func EnsureWith(ctx context.Context, repo, dataDir, project, unitID string, opts EnsureOpts) (Tree, error) {
	if repo == "" || unitID == "" {
		return Tree{}, fmt.Errorf("repo and unitID are required")
	}
	if !IsRepo(repo) {
		return Tree{}, fmt.Errorf("not a git repository: %s", repo)
	}

	prefix := opts.BranchPrefix
	if strings.TrimSpace(prefix) == "" {
		prefix = PrefixForUnitID(unitID)
	} else {
		prefix = NormalizePrefix(prefix)
	}
	branch := prefix + unitID
	if !IsManagedBranch(branch) {
		return Tree{}, fmt.Errorf("refuse to ensure unmanaged branch %q", branch)
	}
	path := WorktreePath(dataDir, project, unitID)
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
			errs = append(errs, fmt.Sprintf("refuse to delete unprotected branch %q (want prefix %s or %s)", branch, DiscordBranchPrefix, WebBranchPrefix))
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
		return fmt.Errorf("refuse to delete unprotected remote branch %q (want prefix %s or %s)", branch, DiscordBranchPrefix, WebBranchPrefix)
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
