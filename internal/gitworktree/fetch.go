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

// PrimaryBranchCandidateNames is the ordered short-name list used when
// preferred is empty (after origin/HEAD). Exported so ghpr Actions primary
// resolution can share the same order without import cycles.
var PrimaryBranchCandidateNames = []string{
	"main",
	"master",
	"prod",
	"production",
	"staging",
	"develop",
	"dev",
}

// PrimaryStartRef is the tip new managed worktrees are based on. preferred is
// a short branch name (no origin/, no '/'); empty keeps the heuristic. When
// preferred is non-empty, always returns "origin/<preferred>" (may not exist)
// so string callers get a deterministic ref for git to reject. The commits
// list uses ResolveCommitsRef, which ignores a preferred name that is not a
// commit on that checkout.
func PrimaryStartRef(ctx context.Context, repo, preferred string) string {
	return resolveNewBranchStart(ctx, repo, preferred)
}

// ResolveCommitsRef is the ref the commits list should show. A requested ref
// that is a commit on repo is kept. Otherwise the default is origin/<preferred>
// when that commit exists, else this checkout's own primary: origin/HEAD when
// it points at a commit, else the first existing PrimaryBranchCandidateNames
// tip, else HEAD. A preferred name that is not a commit on this checkout is
// ignored — unlike PrimaryStartRef, which still returns origin/<preferred>.
func ResolveCommitsRef(ctx context.Context, repo, requested, preferred string) string {
	requested = strings.TrimSpace(requested)
	if requested != "" && commitRefExists(ctx, repo, requested) {
		return requested
	}
	preferred = strings.TrimSpace(preferred)
	if preferred != "" && commitRefExists(ctx, repo, "origin/"+preferred) {
		return "origin/" + preferred
	}
	return resolveNewBranchStart(ctx, repo, "")
}

// OriginTrackingRefs lists remote-tracking branches under refs/remotes/origin
// as shorthand origin/<name>, in refname order. origin/HEAD is omitted. Callers
// that render a dropdown should keep the active ref when this returns an error.
func OriginTrackingRefs(ctx context.Context, repo string) ([]string, error) {
	out, err := gitOutput(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	var refs []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "origin/HEAD" || !strings.HasPrefix(line, "origin/") {
			continue
		}
		refs = append(refs, line)
	}
	return refs, nil
}

// resolveNewBranchStart picks the tip for a newly created managed branch.
// Prefers origin's default branch (or common origin/* candidates) so worktrees
// are not based on a stale local HEAD after fetch. Falls back to HEAD.
func resolveNewBranchStart(ctx context.Context, repo, preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		return "origin/" + preferred
	}
	if out, err := gitOutput(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		ref = strings.TrimPrefix(ref, "refs/remotes/")
		if ref != "" && commitRefExists(ctx, repo, ref) {
			return ref
		}
	}
	for _, name := range PrimaryBranchCandidateNames {
		c := "origin/" + name
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
	_, err := gitOutput(ctx, repo, "rev-parse", "--verify", qualifyOriginRef(ref)+"^{commit}")
	return err == nil
}

// qualifyOriginRef rewrites a shorthand origin/<name> to refs/remotes/origin/<name>.
// Git stores a fetch destination or revision "origin/main" as the local branch
// refs/heads/origin/main, which then makes the shorthand ambiguous and hides
// the remote-tracking ref. HEAD and already-qualified refs pass through.
func qualifyOriginRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "HEAD" || strings.HasPrefix(ref, "refs/") {
		return ref
	}
	rest, ok := strings.CutPrefix(ref, "origin/")
	if !ok || rest == "" {
		return ref
	}
	return "refs/remotes/origin/" + rest
}

func ambiguousObjectName(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ambiguous")
}

// fetchBeforeCreate runs a short-throttle fetch and returns the start ref for
// worktree add -b. Fetch errors are logged; start ref still resolves from what
// is already on disk.
func fetchBeforeCreate(ctx context.Context, repo, preferred string) string {
	if _, err := MaybeFetch(ctx, repo, CreateFetchThrottle); err != nil {
		log.Printf("gitworktree: fetch before create repo=%s: %v", repo, err)
	}
	return resolveNewBranchStart(ctx, repo, preferred)
}
