package gitworktree

import (
	"context"
	"fmt"
	"strings"
)

// Checkpoint ref namespace: refs/grok-cp/<threadId>/<checkpointId>
// Separate from managed *branch* prefixes (IsManagedBranch).

const checkpointRefPrefix = "refs/grok-cp/"

// CheckpointRef builds the full ref name for a thread-scoped checkpoint.
func CheckpointRef(threadID, id string) string {
	threadID = sanitizePathSegment(threadID)
	id = sanitizePathSegment(id)
	return checkpointRefPrefix + threadID + "/" + id
}

// ParseCheckpointRef returns threadID and id when ref is a valid grok-cp ref.
func ParseCheckpointRef(ref string) (threadID, id string, ok bool) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, checkpointRefPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(ref, checkpointRefPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsCheckpointRefForThread is true when ref is exactly refs/grok-cp/<threadID>/<id>.
func IsCheckpointRefForThread(ref, threadID string) bool {
	tid, _, ok := ParseCheckpointRef(ref)
	if !ok {
		return false
	}
	return tid == sanitizePathSegment(threadID)
}

// CreateCheckpointRef writes refs/grok-cp/<threadID>/<id> → HEAD (or sha if non-empty).
func CreateCheckpointRef(ctx context.Context, cwd, threadID, id, sha string) (resolvedSHA, ref string, err error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", "", fmt.Errorf("cwd required")
	}
	ref = CheckpointRef(threadID, id)
	if sha == "" {
		sha, err = gitOutput(ctx, cwd, "rev-parse", "HEAD")
		if err != nil {
			return "", "", err
		}
	} else {
		sha, err = gitOutput(ctx, cwd, "rev-parse", "--verify", sha)
		if err != nil {
			return "", "", err
		}
	}
	if err := runGit(ctx, cwd, "update-ref", ref, sha); err != nil {
		return "", "", err
	}
	return sha, ref, nil
}

// ResolveRefSHA returns the commit SHA for a ref (or empty if missing).
func ResolveRefSHA(ctx context.Context, cwd, ref string) (string, error) {
	return gitOutput(ctx, cwd, "rev-parse", "--verify", ref)
}

// HeadSHA returns HEAD commit.
func HeadSHA(ctx context.Context, cwd string) (string, error) {
	return gitOutput(ctx, cwd, "rev-parse", "HEAD")
}

// HeadBranchName returns current branch short name, or empty when detached.
func HeadBranchName(ctx context.Context, cwd string) string {
	out, err := gitOutput(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "HEAD" {
		return ""
	}
	return out
}

// WorkingTreeDirty is true when there are unstaged/staged changes or untracked files.
func WorkingTreeDirty(ctx context.Context, cwd string) (bool, error) {
	out, err := gitOutput(ctx, cwd, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// HardResetToSHA resets the index and working tree to sha (local only; no push).
func HardResetToSHA(ctx context.Context, cwd, sha string) error {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return fmt.Errorf("sha required")
	}
	return runGit(ctx, cwd, "reset", "--hard", sha)
}

// FetchOrigin runs git fetch origin (prune optional via args).
func FetchOrigin(ctx context.Context, cwd string) error {
	return runGit(ctx, cwd, "fetch", "origin", "--prune")
}

// FetchPullHead fetches origin's pull/N/head into FETCH_HEAD so AddDetached
// can check out a PR that is not a local branch.
func FetchPullHead(ctx context.Context, cwd string, number int) error {
	if number <= 0 {
		return fmt.Errorf("pull number required")
	}
	return runGit(ctx, cwd, "fetch", "--no-tags", "origin", fmt.Sprintf("pull/%d/head", number))
}

// MergeOriginBase merges origin/<base> into the current branch.
// On conflict returns err with "conflict" substring and leaves conflicted state.
func MergeOriginBase(ctx context.Context, cwd, base string) error {
	base = strings.TrimSpace(base)
	if base == "" {
		return fmt.Errorf("base branch required")
	}
	return runGit(ctx, cwd, "merge", "--no-edit", "origin/"+base)
}

// ConflictedFiles lists paths with unmerged index entries.
func ConflictedFiles(ctx context.Context, cwd string) ([]string, error) {
	out, err := gitOutput(ctx, cwd, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		// empty when no conflicts / clean
		if strings.TrimSpace(out) == "" {
			return nil, nil
		}
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// AbortMerge runs git merge --abort when mid-merge.
func AbortMerge(ctx context.Context, cwd string) error {
	return runGit(ctx, cwd, "merge", "--abort")
}
