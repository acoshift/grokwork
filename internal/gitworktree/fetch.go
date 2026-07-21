package gitworktree

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CreateFetchThrottle is the min gap between git fetches when creating a new
// worktree. Idle auto-fetch uses the per-project config interval instead.
const CreateFetchThrottle = 5 * time.Second

// In-process throttle: last successful fetch time per main checkout path.
// Shared by idle auto-fetch, worktree create, and manual Commits UI fetch.
var (
	fetchMu   sync.Mutex
	lastFetch = map[string]time.Time{}
)

// NoteFetched records that repo was just fetched (e.g. manual Commits UI fetch)
// so idle auto-fetch / create can skip within their respective intervals.
func NoteFetched(repo string) {
	key := fetchKey(repo)
	if key == "" {
		return
	}
	fetchMu.Lock()
	lastFetch[key] = time.Now()
	fetchMu.Unlock()
}

// MaybeFetch runs git fetch --all --prune when interval > 0 and this repo has
// not been successfully fetched within the interval. interval <= 0 is a no-op.
// ran is true only when a fetch command was executed successfully.
// Concurrent callers for the same repo serialize on the fetch.
func MaybeFetch(ctx context.Context, repo string, interval time.Duration) (ran bool, err error) {
	if interval <= 0 {
		return false, nil
	}
	repo = strings.TrimSpace(repo)
	if repo == "" || !IsRepo(repo) {
		return false, nil
	}
	key := fetchKey(repo)
	if key == "" {
		return false, nil
	}

	fetchMu.Lock()
	defer fetchMu.Unlock()
	if last, ok := lastFetch[key]; ok && time.Since(last) < interval {
		return false, nil
	}
	if err := runGit(ctx, repo, "fetch", "--all", "--prune"); err != nil {
		return false, err
	}
	lastFetch[key] = time.Now()
	return true, nil
}

// resetFetchStateForTest clears the in-process fetch throttle (tests only).
func resetFetchStateForTest() {
	fetchMu.Lock()
	lastFetch = map[string]time.Time{}
	fetchMu.Unlock()
}

func fetchKey(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return filepath.Clean(repo)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// resolveNewBranchStart picks the tip for a newly created managed branch.
// Prefers origin's default branch (or common origin/* candidates) so worktrees
// are not based on a stale local HEAD after fetch. Falls back to HEAD.
func resolveNewBranchStart(ctx context.Context, repo string) string {
	if out, err := gitOutput(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		ref = strings.TrimPrefix(ref, "refs/remotes/")
		if ref != "" && commitRefExists(ctx, repo, ref) {
			return ref
		}
	}
	for _, c := range []string{
		"origin/main",
		"origin/master",
		"origin/prod",
		"origin/production",
		"origin/staging",
		"origin/develop",
		"origin/dev",
	} {
		if commitRefExists(ctx, repo, c) {
			return c
		}
	}
	return "HEAD"
}

func commitRefExists(ctx context.Context, repo, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	_, err := gitOutput(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
	return err == nil
}

// fetchBeforeCreate runs a short-throttle fetch and returns the start ref for
// worktree add -b. Fetch errors are logged; start ref still resolves from what
// is already on disk.
func fetchBeforeCreate(ctx context.Context, repo string) string {
	if _, err := MaybeFetch(ctx, repo, CreateFetchThrottle); err != nil {
		log.Printf("gitworktree: fetch before create repo=%s: %v", repo, err)
	}
	return resolveNewBranchStart(ctx, repo)
}
