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
	Created bool // remote branch was absent; a regular push created it
}

// ForcePush points origin/<target> at SHA. When that remote branch does not
// exist, a regular push creates it. When it does, fast-forward when possible;
// otherwise --force-with-lease= with an explicit expected SHA. It never
// checkouts, resets, or switches the shared main repo, never uses a bare
// force flag, and never uses a + refspec.
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
	fetchedOK := fetchErr == nil
	if !commitRefExists(ctx, repo, sha) && isShallowRepo(ctx, repo) {
		if runGit(ctx, repo, "fetch", "origin", "--prune", "--unshallow") == nil {
			fetchedOK = true
		}
	}
	if !commitRefExists(ctx, repo, sha) {
		if !fetchedOK {
			return out, scrubCherryPickErr(fmt.Errorf("fetch origin: %w", fetchErr), repo)
		}
		return out, fmt.Errorf("cannot resolve %s", shortSHA(sha))
	}

	full, err := resolveForcePushCommit(ctx, repo, sha)
	if err != nil {
		return out, scrubCherryPickErr(err, repo)
	}
	out.ToSHA = full

	fromSHA, present := remoteTargetCommit(ctx, repo, target)
	if present && fromSHA == "" && isShallowRepo(ctx, repo) {
		if runGit(ctx, repo, "fetch", "origin", "--prune", "--unshallow") == nil {
			fetchedOK = true
		}
		fromSHA, present = remoteTargetCommit(ctx, repo, target)
	}
	if fromSHA != "" {
		out.FromSHA = fromSHA
	}

	// A configured primary is refused even when origin/<name> is absent, so
	// the create path cannot invent the primary on a first push.
	if pref := strings.TrimSpace(opts.PreferredPrimary); pref != "" && pref == target {
		return out, fmt.Errorf("refuse to force-push onto project primary %q", target)
	}
	if name, _, rerr := ResolvePrimaryBranch(ctx, repo, strings.TrimSpace(opts.PreferredPrimary)); rerr == nil && name == target {
		return out, fmt.Errorf("refuse to force-push onto project primary %q", target)
	}

	if present && fromSHA == "" {
		if !fetchedOK {
			return out, scrubCherryPickErr(fmt.Errorf("fetch origin: %w", fetchErr), repo)
		}
		return out, fmt.Errorf("cannot resolve origin/%s", target)
	}
	if !present {
		if !fetchedOK {
			return out, scrubCherryPickErr(fmt.Errorf("fetch origin: %w", fetchErr), repo)
		}
		// No observed tip, so this must not lease or overwrite diverged history.
		// A branch that appears in the race is fast-forwarded or rejected.
		if err := pushTarget(ctx, repo, full, target, "", true); err != nil {
			return out, scrubCherryPickErr(fmt.Errorf("push to %s rejected while creating it: %w", target, err), repo)
		}
		out.Created = true
		_ = runGit(ctx, repo, "fetch", "origin", target+":refs/remotes/origin/"+target)
		NoteFetched(repo)
		return out, nil
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

// remoteTargetCommit reports the commit refs/remotes/origin/<target> peels to.
// present is false when that remote-tracking ref does not exist. present is
// true and sha is empty when the ref exists but does not peel to a commit
// object this checkout has — that is not "absent", and must not be created over.
func remoteTargetCommit(ctx context.Context, repo, target string) (sha string, present bool) {
	ref := qualifyOriginRef("origin/" + target)
	raw, err := gitOutput(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", false
	}
	peeled, err := gitOutput(ctx, repo, "rev-parse", "--verify", strings.TrimSpace(raw)+"^{commit}")
	if err != nil || strings.TrimSpace(peeled) == "" {
		return "", true
	}
	return strings.TrimSpace(peeled), true
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
