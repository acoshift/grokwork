package gitworktree

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// MaxCherryPickSHAs caps one CherryPick call.
const MaxCherryPickSHAs = 20

var hexSHARe = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// IsHexSHA reports whether s is a git SHA (abbreviated or full), not a ref name.
func IsHexSHA(s string) bool {
	return hexSHARe.MatchString(strings.TrimSpace(s))
}

// CherryPickCheckoutPath is the ephemeral worktree directory for one pick.
// Lives under dataDir/cherrypick, never worktreesRoot (idle sweeper) or
// deploys/checkouts.
func CherryPickCheckoutPath(dataDir, project, id string) string {
	return filepath.Join(strings.TrimSpace(dataDir), "cherrypick",
		sanitizePathSegment(project), sanitizePathSegment(id))
}

// CherryPickOpts is one cherry-pick onto a long-lived target branch.
type CherryPickOpts struct {
	Repo             string
	Checkout         string // unique path from CherryPickCheckoutPath
	Target           string // short branch name
	SHAs             []string
	PreferredPrimary string // config primary; empty = heuristic
}

// CherryPickResult is the outcome of CherryPick.
type CherryPickResult struct {
	Target  string
	FromSHA string
	ToSHA   string
	Picked  []string
	Skipped []string
	Noop    bool
}

var (
	cherryPickMu    sync.Mutex
	cherryPickLocks = map[string]*sync.Mutex{}
)

func lockCherryPickRepo(repo string) func() {
	key := fetchKey(repo)
	if key == "" {
		key = strings.TrimSpace(repo)
	}
	cherryPickMu.Lock()
	m := cherryPickLocks[key]
	if m == nil {
		m = new(sync.Mutex)
		cherryPickLocks[key] = m
	}
	cherryPickMu.Unlock()
	m.Lock()
	return m.Unlock
}

// CherryPick applies SHAs onto origin/<target> in an ephemeral detached
// worktree and fast-forward-pushes. It never force-pushes and never creates
// a missing remote branch.
func CherryPick(ctx context.Context, opts CherryPickOpts) (CherryPickResult, error) {
	var out CherryPickResult
	repo := strings.TrimSpace(opts.Repo)
	checkout := strings.TrimSpace(opts.Checkout)
	target := strings.TrimSpace(opts.Target)
	out.Target = target

	if repo == "" || !IsRepo(repo) {
		return out, fmt.Errorf("main repo is not a git repository")
	}
	if checkout == "" {
		return out, fmt.Errorf("checkout path is required")
	}
	if err := validateCherryPickTarget(target); err != nil {
		return out, err
	}
	if filepath.Clean(repo) == filepath.Clean(checkout) {
		return out, fmt.Errorf("refuse to cherry-pick in the main repo checkout")
	}

	shas, err := normalizeCherryPickSHAs(opts.SHAs)
	if err != nil {
		return out, err
	}

	unlock := lockCherryPickRepo(repo)
	defer unlock()

	fetchErr := runGit(ctx, repo, "fetch", "origin", "--prune")
	if missing := cherryPickMissing(ctx, repo, target, shas); len(missing) > 0 {
		if isShallowRepo(ctx, repo) {
			_ = runGit(ctx, repo, "fetch", "origin", "--prune", "--unshallow")
			missing = cherryPickMissing(ctx, repo, target, shas)
		}
		if len(missing) > 0 {
			if fetchErr != nil {
				return out, scrubCherryPickErr(fmt.Errorf("fetch origin: %w", fetchErr), repo, checkout)
			}
			return out, fmt.Errorf("cannot resolve %s", strings.Join(missing, ", "))
		}
	}

	remoteRef := "origin/" + target
	fromSHA, err := gitOutput(ctx, repo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil || strings.TrimSpace(fromSHA) == "" {
		return out, fmt.Errorf("target branch %q not found as %s after fetch (refusing push that would create it)", target, remoteRef)
	}
	out.FromSHA = strings.TrimSpace(fromSHA)

	if name, _, rerr := ResolvePrimaryBranch(ctx, repo, strings.TrimSpace(opts.PreferredPrimary)); rerr == nil && name == target {
		return out, fmt.Errorf("refuse to cherry-pick onto project primary %q", target)
	}

	resolved, skipped, err := resolveCherryPickSHAs(ctx, repo, shas, out.FromSHA)
	if err != nil {
		return out, scrubCherryPickErr(err, repo, checkout)
	}
	out.Skipped = skipped
	if len(resolved) == 0 {
		out.ToSHA = out.FromSHA
		out.Noop = true
		return out, nil
	}

	ordered, err := orderOldestFirst(ctx, repo, resolved)
	if err != nil {
		return out, scrubCherryPickErr(err, repo, checkout)
	}

	if err := AddDetached(ctx, repo, checkout, out.FromSHA); err != nil {
		return out, scrubCherryPickErr(err, repo, checkout)
	}
	defer func() { _ = Remove(ctx, repo, checkout, "") }()

	for _, sha := range ordered {
		if err := applyCherryPick(ctx, checkout, sha); err != nil {
			files := conflictFiles(ctx, checkout)
			empty := isEmptyCherryPick(err) && len(files) == 0
			_ = runGit(ctx, checkout, "cherry-pick", "--abort")
			if empty {
				out.Skipped = append(out.Skipped, shortSHA(sha))
				continue
			}
			short := shortSHA(sha)
			if len(files) > 0 {
				return out, fmt.Errorf("conflict cherry-picking %s onto %s: %s", short, target, strings.Join(files, ", "))
			}
			return out, scrubCherryPickErr(fmt.Errorf("cherry-pick %s onto %s: %w", short, target, err), repo, checkout)
		}
		out.Picked = append(out.Picked, shortSHA(sha))
	}

	newHEAD, err := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return out, scrubCherryPickErr(fmt.Errorf("HEAD: %w", err), repo, checkout)
	}
	newHEAD = strings.TrimSpace(newHEAD)
	out.ToSHA = newHEAD
	if newHEAD == "" || newHEAD == out.FromSHA {
		out.ToSHA = out.FromSHA
		out.Noop = true
		return out, nil
	}

	dest := "refs/heads/" + target
	if err := runGit(ctx, repo, "push", "origin", newHEAD+":"+dest); err != nil {
		return out, scrubCherryPickErr(fmt.Errorf("push to %s rejected (non-fast-forward or protected): %w", target, err), repo, checkout)
	}
	_ = runGit(ctx, repo, "fetch", "origin", target+":refs/remotes/origin/"+target)
	NoteFetched(repo)
	return out, nil
}

func validateCherryPickTarget(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("target branch is empty")
	}
	if name == "HEAD" {
		return fmt.Errorf("target branch cannot be HEAD")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "..") ||
		strings.HasPrefix(name, "-") || name == "." || name == ".." {
		return fmt.Errorf("invalid target branch %q", name)
	}
	if IsManagedBranch(name) {
		return fmt.Errorf("refuse to cherry-pick onto managed branch %q", name)
	}
	return nil
}

func normalizeCherryPickSHAs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one commit sha is required")
	}
	var out []string
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !IsHexSHA(s) {
			return nil, fmt.Errorf("invalid commit sha %q", s)
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one commit sha is required")
	}
	if len(out) > MaxCherryPickSHAs {
		return nil, fmt.Errorf("at most %d commits per cherry-pick", MaxCherryPickSHAs)
	}
	return out, nil
}

func cherryPickMissing(ctx context.Context, repo, target string, shas []string) []string {
	var missing []string
	if !commitRefExists(ctx, repo, "origin/"+target) {
		missing = append(missing, "origin/"+target)
	}
	for _, s := range shas {
		if !commitRefExists(ctx, repo, s) {
			missing = append(missing, shortSHA(s))
		}
	}
	return missing
}

func resolveCherryPickSHAs(ctx context.Context, repo string, shas []string, targetHEAD string) (resolved, skipped []string, err error) {
	seen := map[string]struct{}{}
	for _, s := range shas {
		full, rerr := gitOutput(ctx, repo, "rev-parse", "--verify", s+"^{commit}")
		if rerr != nil {
			return nil, skipped, fmt.Errorf("unknown commit %s", shortSHA(s))
		}
		full = strings.TrimSpace(full)
		if full == "" {
			return nil, skipped, fmt.Errorf("unknown commit %s", shortSHA(s))
		}
		if _, err := gitOutput(ctx, repo, "rev-parse", "--verify", full+"^2"); err == nil {
			return nil, skipped, fmt.Errorf("refuse to cherry-pick merge commit %s", shortSHA(full))
		}
		if _, ok := seen[full]; ok {
			continue
		}
		seen[full] = struct{}{}
		if runGit(ctx, repo, "merge-base", "--is-ancestor", full, targetHEAD) == nil {
			skipped = append(skipped, shortSHA(full))
			continue
		}
		resolved = append(resolved, full)
	}
	return resolved, skipped, nil
}

func orderOldestFirst(ctx context.Context, repo string, shas []string) ([]string, error) {
	args := append([]string{"rev-list", "--no-walk", "--reverse", "--date-order"}, shas...)
	out, err := gitOutput(ctx, repo, args...)
	if err != nil {
		return nil, fmt.Errorf("order commits: %w", err)
	}
	var ordered []string
	seen := map[string]struct{}{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		ordered = append(ordered, line)
	}
	for _, s := range shas {
		if _, ok := seen[s]; !ok {
			ordered = append(ordered, s)
		}
	}
	slices.SortStableFunc(ordered, func(a, b string) int {
		if a == b {
			return 0
		}
		if runGit(ctx, repo, "merge-base", "--is-ancestor", a, b) == nil {
			return -1
		}
		if runGit(ctx, repo, "merge-base", "--is-ancestor", b, a) == nil {
			return 1
		}
		return 0
	})
	return ordered, nil
}

// applyCherryPick fast-forwards when sha's parent is HEAD (keeps the original
// SHA; git forbids combining --ff with -x), otherwise cherry-picks with -x.
func applyCherryPick(ctx context.Context, checkout, sha string) error {
	head, herr := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	parent, perr := gitOutput(ctx, checkout, "rev-parse", sha+"^")
	if herr == nil && perr == nil && strings.TrimSpace(head) == strings.TrimSpace(parent) {
		return runGit(ctx, checkout, "merge", "--ff-only", sha)
	}
	return runGit(ctx, checkout, "cherry-pick", "-x", sha)
}

func isShallowRepo(ctx context.Context, repo string) bool {
	out, err := gitOutput(ctx, repo, "rev-parse", "--is-shallow-repository")
	return err == nil && strings.TrimSpace(out) == "true"
}

func isEmptyCherryPick(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "now empty") ||
		strings.Contains(msg, "nothing to commit") ||
		strings.Contains(msg, "empty commit")
}

func conflictFiles(ctx context.Context, checkout string) []string {
	out, err := gitOutput(ctx, checkout, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func scrubCherryPickErr(err error, paths ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, p, "[path]")
	}
	return fmt.Errorf("%s", msg)
}
