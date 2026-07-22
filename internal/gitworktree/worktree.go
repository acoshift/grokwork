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
	// BranchPrefix is the default for new Discord-thread (non-web) units.
	BranchPrefix = "grokwork/"

	// WebBranchPrefix is for web-native unit ids (w_*).
	WebBranchPrefix = "grok/web/"

	// DiscordBranchPrefix is the legacy Discord prefix. Still recognized by
	// IsManagedBranch / PrefixFromBranch so in-flight sessions, worktrees, and
	// PRs on grok/discord/* keep working until they finish.
	DiscordBranchPrefix = "grok/discord/"
)

// ManagedPrefixes lists every prefix the bot may create or delete (newest first).
var ManagedPrefixes = []string{
	BranchPrefix,
	WebBranchPrefix,
	DiscordBranchPrefix,
}

// EnsureOpts selects which managed branch prefix Ensure/Cleanup use.
// Empty BranchPrefix means PrefixForUnitID (BranchPrefix for Discord units).
type EnsureOpts struct {
	BranchPrefix string
}

// BranchName returns the default managed branch for a Discord unit id (thread snowflake).
func BranchName(unitID string) string {
	return BranchPrefix + unitID
}

// BranchNameWithPrefix returns prefix+unitID after normalizing the prefix.
func BranchNameWithPrefix(prefix, unitID string) string {
	return NormalizePrefix(prefix) + unitID
}

// NormalizePrefix returns a known managed prefix; empty or unknown → BranchPrefix.
// Legacy Discord and web prefixes are preserved when explicitly requested so
// existing sessions keep their branch name across Ensure/Cleanup.
func NormalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	switch prefix {
	case BranchPrefix, strings.TrimSuffix(BranchPrefix, "/"), "":
		return BranchPrefix
	case WebBranchPrefix, strings.TrimSuffix(WebBranchPrefix, "/"):
		return WebBranchPrefix
	case DiscordBranchPrefix, strings.TrimSuffix(DiscordBranchPrefix, "/"):
		return DiscordBranchPrefix
	default:
		if strings.HasPrefix(prefix, "grok/web") {
			return WebBranchPrefix
		}
		if strings.HasPrefix(prefix, "grok/discord") {
			return DiscordBranchPrefix
		}
		if strings.HasPrefix(prefix, "grokwork") {
			return BranchPrefix
		}
		return BranchPrefix
	}
}

// PrefixForUnitID chooses branch prefix from unit id form (w_* → web; else BranchPrefix).
func PrefixForUnitID(unitID string) string {
	if IsWebUnitID(unitID) {
		return WebBranchPrefix
	}
	return BranchPrefix
}

// PrefixFromBranch returns the managed prefix for a branch, or empty if unmanaged.
// Recognizes the current default plus legacy Discord/web prefixes.
func PrefixFromBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	for _, p := range ManagedPrefixes {
		if strings.HasPrefix(branch, p) {
			rest := branch[len(p):]
			if rest != "" && rest != "." && rest != ".." && !strings.Contains(rest, "..") && !strings.HasPrefix(rest, "/") {
				return p
			}
		}
	}
	return ""
}

// managedPrefixHint is for refuse-to-delete error messages.
func managedPrefixHint() string {
	return strings.Join(ManagedPrefixes, " or ")
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
// Prefer sessionCwd when it is still a real worktree root; otherwise the
// canonical path under dataDir. This heals sessions that stored absolute paths
// under an old data directory (e.g. …/grok-discord/data/worktrees/… after a
// rename to grokwork). onDisk is true only when the path is a git worktree root
// (not merely a directory nested inside another repo — worktrees live under
// the bot dataDir, so empty dirs would otherwise resolve to the bot's own repo).
// When nothing usable is on disk, returns the canonical path with onDisk=false.
func ResolveSessionWorktreePath(dataDir, project, unitID, sessionCwd, mainCwd string) (path string, onDisk bool) {
	canonical := WorktreePath(dataDir, project, unitID)
	sessionCwd = strings.TrimSpace(sessionCwd)
	mainCwd = strings.TrimSpace(mainCwd)

	// Live session path wins when it is a real worktree root (not main checkout).
	if sessionCwd != "" && sessionCwd != mainCwd && IsRepo(sessionCwd) {
		return sessionCwd, true
	}
	if IsRepo(canonical) {
		return canonical, true
	}
	return canonical, false
}

// FindOnDiskByUnitID returns the first on-disk worktree whose path segment is unitID.
// Used when session metadata lost project/cwd but the worktree still exists under dataDir.
func FindOnDiskByUnitID(dataDir, unitID string) (OnDisk, bool) {
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return OnDisk{}, false
	}
	list, err := ListOnDisk(dataDir)
	if err != nil {
		return OnDisk{}, false
	}
	for _, d := range list {
		if d.ThreadID == unitID {
			return d, true
		}
	}
	return OnDisk{}, false
}

// IsRepo reports whether dir is a git worktree root (not merely inside a parent
// repo). Nested empty dirs under the bot's own checkout must return false so
// callers never run git and accidentally diff the bot repository.
//
// Uses a filesystem probe only (no git subprocess): a root has a .git directory
// (normal checkout) or a .git file (linked worktree / submodule). Nested paths
// have neither. ListWorktrees / SSE fingerprints call this for every path, so
// spawning git here made the worktrees page and live revs measurably slow.
func IsRepo(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return st.IsDir() || st.Mode().IsRegular()
}

// CleanupIfPRDone removes the worktree/branch when the PR is merged or closed.
// Uses default branch naming for unitID (BranchPrefix / web).
// Missing gh → cleaned=false with err set.
func CleanupIfPRDone(ctx context.Context, repo, dataDir, project, unitID string) (cleaned bool, state string, err error) {
	return CleanupIfPRDoneWith(ctx, repo, dataDir, project, unitID, EnsureOpts{})
}

// CleanupIfPRDoneWith is like CleanupIfPRDone but honors BranchPrefix (or empty → PrefixForUnitID).
// If branch is already known, pass EnsureOpts{BranchPrefix: PrefixFromBranch(branch)} or use
// the unit id form so PrefixForUnitID applies when BranchPrefix is empty and unit is w_*.
// When the preferred branch is absent, also tries other managed prefixes for unitID so
// legacy grok/discord/* trees are cleaned after the default prefix change.
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
	branch := resolveExistingManagedBranch(ctx, repo, dataDir, project, unitID, prefix)
	path := WorktreePath(dataDir, project, unitID)

	hasPath := false
	if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
		hasPath = true
	}
	hasBranch := branch != "" && branchExists(ctx, repo, branch)
	if !hasPath && !hasBranch {
		return false, "", nil
	}
	if branch == "" {
		// Path exists but no managed branch found — still try preferred name for PR check.
		branch = prefix + unitID
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

// AddDetached creates (or reuses) a detached worktree at commit sha.
// No branch is created — suitable for ephemeral read-only checkouts (e.g. commit review).
// path is the worktree directory (must not be the main repo root).
func AddDetached(ctx context.Context, repo, path, sha string) error {
	repo = strings.TrimSpace(repo)
	path = strings.TrimSpace(path)
	sha = strings.TrimSpace(sha)
	if repo == "" || path == "" || sha == "" {
		return fmt.Errorf("repo, path, and sha are required")
	}
	if !IsRepo(repo) {
		return fmt.Errorf("not a git repository: %s", repo)
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Clean(absRepo) == filepath.Clean(absPath) {
		return fmt.Errorf("refuse to add detached worktree at main repo path")
	}

	if ok, err := isUsableWorktree(ctx, repo, path); err != nil {
		return err
	} else if ok {
		// Reuse only when HEAD matches the requested commit.
		want, werr := gitOutput(ctx, repo, "rev-parse", sha)
		have, herr := gitOutput(ctx, path, "rev-parse", "HEAD")
		if werr == nil && herr == nil && strings.TrimSpace(want) == strings.TrimSpace(have) {
			short := strings.TrimSpace(have)
			if len(short) > 12 {
				short = short[:12]
			}
			log.Printf("gitworktree: reuse detached path=%s sha=%s", path, short)
			return nil
		}
		log.Printf("gitworktree: replacing detached path=%s (wrong HEAD)", path)
		_ = removeWorktreeAtPath(ctx, repo, path)
	} else if _, err := os.Stat(path); err == nil {
		_ = removeWorktreeAtPath(ctx, repo, path)
	} else {
		_ = pruneStaleWorktrees(ctx, repo)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if err := runGit(ctx, repo, "worktree", "add", "--detach", path, sha); err != nil {
		return fmt.Errorf("git worktree add --detach: %w", err)
	}
	log.Printf("gitworktree: created detached path=%s sha=%s repo=%s", path, sha, repo)
	return nil
}

// Ensure creates or reuses a default-prefix worktree for unitID.
func Ensure(ctx context.Context, repo, dataDir, project, unitID string) (Tree, error) {
	return EnsureWith(ctx, repo, dataDir, project, unitID, EnsureOpts{})
}

// EnsureWith creates or reuses a worktree; BranchPrefix empty uses PrefixForUnitID(unitID).
// When reusing an on-disk worktree, HEAD is preferred if it is a managed branch so
// legacy grok/discord/* trees are not renamed to the new default by accident.
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
	// Prefer an already-existing managed branch for this unit (legacy grok/discord/*
	// after the prefix rename) over creating a parallel BranchPrefix branch.
	branch := resolveExistingManagedBranch(ctx, repo, dataDir, project, unitID, prefix)
	if !IsManagedBranch(branch) {
		return Tree{}, fmt.Errorf("refuse to ensure unmanaged branch %q", branch)
	}
	path := WorktreePath(dataDir, project, unitID)
	t := Tree{Path: path, Branch: branch, Repo: repo}

	if ok, err := isUsableWorktree(ctx, repo, path); err != nil {
		return Tree{}, err
	} else if ok {
		if cur := headBranch(ctx, path); IsManagedBranch(cur) {
			// Keep the worktree's real branch (e.g. legacy grok/discord/<id>).
			branch = cur
			t.Branch = cur
		}
		log.Printf("gitworktree: reuse path=%s branch=%s", path, branch)
		return t, nil
	}

	// Drop an unusable on-disk tree, or a stale git registration whose folder
	// was deleted outside the bot (blocks "worktree add" / "already checked out").
	if _, err := os.Stat(path); err == nil {
		log.Printf("gitworktree: removing unusable path %s", path)
		_ = Remove(ctx, repo, path, "")
	} else {
		_ = pruneStaleWorktrees(ctx, repo)
		_ = removeRegisteredWorktreesForBranch(ctx, repo, branch)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Tree{}, fmt.Errorf("mkdir worktree parent: %w", err)
	}

	// New branch: short-throttle fetch + start from origin/* (not stale local HEAD).
	// Existing branch: re-attach only (no fetch). Idle background fetch keeps
	// remotes warm; create still fetches unless done within CreateFetchThrottle.
	var err error
	if branchExists(ctx, repo, branch) {
		_ = pruneStaleWorktrees(ctx, repo)
		_ = removeRegisteredWorktreesForBranch(ctx, repo, branch)
		err = runGit(ctx, repo, "worktree", "add", path, branch)
	} else {
		start := fetchBeforeCreate(ctx, repo)
		err = runGit(ctx, repo, "worktree", "add", "-b", branch, path, start)
		if err != nil {
			// Fallback to HEAD if origin ref vanished mid-flight.
			if start != "HEAD" {
				log.Printf("gitworktree: start %s failed (%v); retrying with HEAD", start, err)
				err = runGit(ctx, repo, "worktree", "add", "-b", branch, path, "HEAD")
			}
		}
	}
	if err != nil {
		return Tree{}, fmt.Errorf("git worktree add: %w", err)
	}

	log.Printf("gitworktree: created path=%s branch=%s repo=%s", path, branch, repo)
	return t, nil
}

func Remove(ctx context.Context, repo, path, branch string) error {
	var errs []string

	if path != "" {
		if err := removeWorktreeAtPath(ctx, repo, path); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// Folder deleted outside git still leaves an admin entry under
	// .git/worktrees/; branch -D then fails with "used by worktree at …".
	// Prune those, and force-remove any remaining registration for this branch.
	if repo != "" {
		_ = pruneStaleWorktrees(ctx, repo)
		if branch != "" && IsManagedBranch(branch) {
			_ = removeRegisteredWorktreesForBranch(ctx, repo, branch)
			_ = pruneStaleWorktrees(ctx, repo)
		}
	}

	if branch != "" && repo != "" {
		if !IsManagedBranch(branch) {
			errs = append(errs, fmt.Sprintf("refuse to delete unprotected branch %q (want prefix %s)", branch, managedPrefixHint()))
		} else if branchExists(ctx, repo, branch) {
			if err := runGit(ctx, repo, "branch", "-D", branch); err != nil {
				// Last chance: registration may have been mid-prune.
				_ = pruneStaleWorktrees(ctx, repo)
				_ = removeRegisteredWorktreesForBranch(ctx, repo, branch)
				if err2 := runGit(ctx, repo, "branch", "-D", branch); err2 != nil {
					errs = append(errs, fmt.Sprintf("delete branch %s: %v", branch, err2))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// removeWorktreeAtPath force-removes a linked worktree directory if it exists.
func removeWorktreeAtPath(ctx context.Context, repo, path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if err := runGit(ctx, repo, "worktree", "remove", "--force", path); err != nil {
		_ = pruneStaleWorktrees(ctx, repo)
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return fmt.Errorf("remove path: %v (git: %v)", rmErr, err)
		}
		_ = pruneStaleWorktrees(ctx, repo)
	}
	return nil
}

// pruneStaleWorktrees drops admin entries for worktree paths that no longer exist.
func pruneStaleWorktrees(ctx context.Context, repo string) error {
	if repo == "" {
		return nil
	}
	// --expire=now so locked/stale entries whose directory is gone are cleared
	// immediately (default may wait for gc.worktreePruneExpire).
	return runGit(ctx, repo, "worktree", "prune", "--expire", "now")
}

// linkedWorktree is one entry from `git worktree list --porcelain`.
type linkedWorktree struct {
	Path   string
	Branch string // short name without refs/heads/, empty if detached
}

// listLinkedWorktrees returns linked (non-main) worktrees for repo.
func listLinkedWorktrees(ctx context.Context, repo string) ([]linkedWorktree, error) {
	out, err := gitOutput(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var (
		list []linkedWorktree
		cur  linkedWorktree
		have bool
	)
	flush := func() {
		if !have || cur.Path == "" {
			cur = linkedWorktree{}
			have = false
			return
		}
		// Main worktree path equals the repo root; skip it — never remove it.
		list = append(list, cur)
		cur = linkedWorktree{}
		have = false
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			if have {
				flush()
			}
			cur.Path = val
			have = true
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		}
	}
	flush()

	mainAbs, err := filepath.Abs(repo)
	if err != nil {
		return list, nil
	}
	if resolved, err := filepath.EvalSymlinks(mainAbs); err == nil {
		mainAbs = resolved
	}
	mainAbs = filepath.Clean(mainAbs)

	filtered := list[:0]
	for _, wt := range list {
		wtAbs, err := filepath.Abs(wt.Path)
		if err != nil {
			filtered = append(filtered, wt)
			continue
		}
		if resolved, err := filepath.EvalSymlinks(wtAbs); err == nil {
			wtAbs = resolved
		}
		if filepath.Clean(wtAbs) == mainAbs {
			continue
		}
		filtered = append(filtered, wt)
	}
	return filtered, nil
}

// removeRegisteredWorktreesForBranch force-removes every linked worktree that
// still has branch checked out (including paths that no longer exist on disk).
func removeRegisteredWorktreesForBranch(ctx context.Context, repo, branch string) error {
	if repo == "" || branch == "" {
		return nil
	}
	list, err := listLinkedWorktrees(ctx, repo)
	if err != nil {
		return err
	}
	var first error
	for _, wt := range list {
		if wt.Branch != branch {
			continue
		}
		if _, statErr := os.Stat(wt.Path); statErr == nil {
			if err := runGit(ctx, repo, "worktree", "remove", "--force", wt.Path); err != nil {
				_ = os.RemoveAll(wt.Path)
				if first == nil {
					first = err
				}
			}
			continue
		}
		// Path gone: prune should clear it; also try remove for older git.
		if err := runGit(ctx, repo, "worktree", "remove", "--force", wt.Path); err != nil && first == nil {
			first = err
		}
	}
	return first
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
	var abs string
	var err error
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs, err = filepath.Abs(filepath.Join(base, p))
		if err != nil {
			return "", err
		}
	}
	// Resolve /var → /private/var (macOS) so main vs worktree common-dir compare equal.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
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

// headBranch returns the current branch name for a worktree, or empty.
func headBranch(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	out, err := gitOutput(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// resolveExistingManagedBranch picks the branch for unitID that already exists
// (preferred prefix first, then other managed prefixes, then worktree HEAD).
// Returns preferred+unitID when nothing exists yet (caller creates it).
func resolveExistingManagedBranch(ctx context.Context, repo, dataDir, project, unitID, preferredPrefix string) string {
	preferred := preferredPrefix + unitID
	if branchExists(ctx, repo, preferred) {
		return preferred
	}
	path := WorktreePath(dataDir, project, unitID)
	if cur := headBranch(ctx, path); IsManagedBranch(cur) && strings.HasSuffix(cur, "/"+unitID) {
		return cur
	}
	for _, p := range ManagedPrefixes {
		if p == preferredPrefix {
			continue
		}
		cand := p + unitID
		if branchExists(ctx, repo, cand) {
			return cand
		}
	}
	return preferred
}

func deleteRemoteBranch(ctx context.Context, repo, branch string) error {
	if !IsManagedBranch(branch) {
		return fmt.Errorf("refuse to delete unprotected remote branch %q (want prefix %s)", branch, managedPrefixHint())
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
