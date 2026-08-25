package gitworktree

import (
	"context"
	"fmt"
	"strings"
)

// ForcePushOpts is one SHA pointed at a long-lived target branch.
type ForcePushOpts struct {
	Repo             string
	Target           string // short branch name
	SHA              string
	PreferredPrimary string // config primary; empty = heuristic
}

// ForcePushResult is the outcome of ForcePush.
type ForcePushResult struct {
	Target  string
	FromSHA string
	ToSHA   string
	Noop    bool
	Forced  bool // true only when the lease (non-ff) path ran
}

// ForcePush points origin/<target> at SHA. Fast-forward when possible;
// otherwise --force-with-lease= with an explicit expected SHA. It never
// checkouts, resets, or switches the shared main repo, never uses a bare
// force flag, never uses a + refspec, and never creates a missing remote branch.
func ForcePush(ctx context.Context, opts ForcePushOpts) (ForcePushResult, error) {
	var out ForcePushResult
	repo := strings.TrimSpace(opts.Repo)
	target := strings.TrimSpace(opts.Target)
	out.Target = target

	if repo == "" || !IsRepo(repo) {
		return out, fmt.Errorf("main repo is not a git repository")
	}
	if err := validateCherryPickTarget(target); err != nil {
		return out, err
	}
	sha, err := normalizeForcePushSHA(opts.SHA)
	if err != nil {
		return out, err
	}

	unlock := lockCherryPickRepo(repo)
	defer unlock()

	fetchErr := runGit(ctx, repo, "fetch", "origin", "--prune")
	if missing := forcePushMissing(ctx, repo, target, sha); len(missing) > 0 {
		if isShallowRepo(ctx, repo) {
			_ = runGit(ctx, repo, "fetch", "origin", "--prune", "--unshallow")
			missing = forcePushMissing(ctx, repo, target, sha)
		}
		if len(missing) > 0 {
			if fetchErr != nil {
				return out, scrubCherryPickErr(fmt.Errorf("fetch origin: %w", fetchErr), repo)
			}
			return out, fmt.Errorf("cannot resolve %s", strings.Join(missing, ", "))
		}
	}

	full, err := resolveForcePushCommit(ctx, repo, sha)
	if err != nil {
		return out, scrubCherryPickErr(err, repo)
	}
	out.ToSHA = full

	remoteRef := "origin/" + target
	fromSHA, err := gitOutput(ctx, repo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil || fromSHA == "" {
		return out, fmt.Errorf("target branch %q not found as %s after fetch (refusing push that would create it)", target, remoteRef)
	}
	out.FromSHA = fromSHA

	if name, _, rerr := ResolvePrimaryBranch(ctx, repo, strings.TrimSpace(opts.PreferredPrimary)); rerr == nil && name == target {
		return out, fmt.Errorf("refuse to force-push onto project primary %q", target)
	}

	if fromSHA == full {
		out.Noop = true
		return out, nil
	}

	ff := runGit(ctx, repo, "merge-base", "--is-ancestor", fromSHA, full) == nil
	if err := pushTarget(ctx, repo, full, target, fromSHA, ff); err != nil {
		kind := "non-fast-forward or protected"
		if !ff {
			kind = "lease failed, non-fast-forward, or protected"
		}
		return out, scrubCherryPickErr(fmt.Errorf("push to %s rejected (%s): %w", target, kind, err), repo)
	}
	out.Forced = !ff
	_ = runGit(ctx, repo, "fetch", "origin", target+":refs/remotes/origin/"+target)
	NoteFetched(repo)
	return out, nil
}

func normalizeForcePushSHA(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", fmt.Errorf("commit sha is required")
	}
	if !IsHexSHA(s) {
		return "", fmt.Errorf("invalid commit sha %q", s)
	}
	return s, nil
}

func forcePushMissing(ctx context.Context, repo, target, sha string) []string {
	var missing []string
	if !commitRefExists(ctx, repo, "origin/"+target) {
		missing = append(missing, "origin/"+target)
	}
	if !commitRefExists(ctx, repo, sha) {
		missing = append(missing, shortSHA(sha))
	}
	return missing
}

func resolveForcePushCommit(ctx context.Context, repo, sha string) (string, error) {
	typ, err := gitOutput(ctx, repo, "cat-file", "-t", sha)
	if err != nil {
		return "", fmt.Errorf("unknown commit %s", shortSHA(sha))
	}
	if typ != "commit" {
		return "", fmt.Errorf("refuse to force-push %s object %s", typ, shortSHA(sha))
	}
	full, err := gitOutput(ctx, repo, "rev-parse", "--verify", sha+"^{commit}")
	if err != nil || full == "" {
		return "", fmt.Errorf("unknown commit %s", shortSHA(sha))
	}
	return full, nil
}

func pushTarget(ctx context.Context, repo, sha, target, fromSHA string, ff bool) error {
	args := forcePushArgs(sha, target, fromSHA, ff)
	return runGit(ctx, repo, args...)
}

// forcePushArgs is the git argv after -C <repo>. FF uses a regular push;
// otherwise explicit --force-with-lease=<ref>:<expect> (never a bare force
// flag, never a + refspec).
func forcePushArgs(sha, target, fromSHA string, ff bool) []string {
	dest := "refs/heads/" + target
	refspec := sha + ":" + dest
	if ff {
		return []string{"push", "origin", refspec}
	}
	return []string{
		"push",
		"--force-with-lease=" + dest + ":" + fromSHA,
		"origin",
		refspec,
	}
}
