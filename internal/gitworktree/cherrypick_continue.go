package gitworktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrTargetMoved means origin/<target> is no longer FromSHA.
var ErrTargetMoved = fmt.Errorf("target branch moved since cherry-pick started")

const maxConflictFileBytes = 512 << 10

// ContinueOpts finishes a parked cherry-pick after the operator resolved files.
type ContinueOpts struct {
	Repo         string
	Checkout     string
	Target       string
	FromSHA      string
	Current      string   // SHA in the sequencer (for Picked/Skipped bookkeeping)
	AfterCurrent []string // remaining SHAs after the one in the sequencer
}

// ContinueCherryPick stages the working tree, --continues the sequencer, applies
// remaining SHAs, and FF-pushes. It never force-pushes.
func ContinueCherryPick(ctx context.Context, opts ContinueOpts) (CherryPickResult, error) {
	var out CherryPickResult
	repo := strings.TrimSpace(opts.Repo)
	checkout := strings.TrimSpace(opts.Checkout)
	target := strings.TrimSpace(opts.Target)
	out.Target = target
	out.FromSHA = strings.TrimSpace(opts.FromSHA)

	if repo == "" || !IsRepo(repo) {
		return out, fmt.Errorf("main repo is not a git repository")
	}
	if checkout == "" || !IsRepo(checkout) {
		return out, fmt.Errorf("checkout is not a git repository")
	}
	if err := validateCherryPickTarget(target); err != nil {
		return out, err
	}

	unlock := lockCherryPickRepo(repo)
	defer unlock()

	_ = runGit(ctx, repo, "fetch", "origin", "--prune")
	remoteRef := "origin/" + target
	cur, err := gitOutput(ctx, repo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return out, fmt.Errorf("target branch %q not found as %s after fetch (refusing push that would create it)", target, remoteRef)
	}
	cur = strings.TrimSpace(cur)
	if out.FromSHA != "" && cur != out.FromSHA {
		return out, ErrTargetMoved
	}

	if SequencerLive(ctx, checkout) {
		files := conflictFiles(ctx, checkout)
		if leftover := leftoverMarkers(ctx, checkout, files); leftover != "" {
			return out, fmt.Errorf("unresolved conflict markers in %s", leftover)
		}
		for _, f := range files {
			if err := runGit(ctx, checkout, "add", "--", f); err != nil {
				return out, scrubCherryPickErr(err, repo, checkout)
			}
		}
		if err := runGitEnv(ctx, checkout, []string{"GIT_EDITOR=true"}, "cherry-pick", "--continue"); err != nil {
			unmerged := conflictFiles(ctx, checkout)
			if isEmptyCherryPick(err) && len(unmerged) == 0 {
				_ = runGit(ctx, checkout, "cherry-pick", "--skip")
				if cur := strings.TrimSpace(opts.Current); cur != "" {
					out.Skipped = append(out.Skipped, shortSHA(cur))
				}
			} else if len(unmerged) > 0 {
				head, _ := gitOutput(ctx, checkout, "rev-parse", "--verify", "CHERRY_PICK_HEAD")
				return out, &ConflictError{
					Target:    target,
					SHA:       strings.TrimSpace(head),
					Files:     unmerged,
					Remaining: append([]string{strings.TrimSpace(head)}, opts.AfterCurrent...),
					Result:    out,
				}
			} else {
				return out, scrubCherryPickErr(fmt.Errorf("cherry-pick --continue: %w", err), repo, checkout)
			}
		} else if cur := strings.TrimSpace(opts.Current); cur != "" {
			out.Picked = append(out.Picked, shortSHA(cur))
		}
	}

	for i, sha := range opts.AfterCurrent {
		sha = strings.TrimSpace(sha)
		if sha == "" {
			continue
		}
		if err := applyCherryPick(ctx, checkout, sha); err != nil {
			files := conflictFiles(ctx, checkout)
			empty := isEmptyCherryPick(err) && len(files) == 0
			if empty {
				_ = runGit(ctx, checkout, "cherry-pick", "--abort")
				out.Skipped = append(out.Skipped, shortSHA(sha))
				continue
			}
			if len(files) == 0 {
				_ = runGit(ctx, checkout, "cherry-pick", "--abort")
				return out, scrubCherryPickErr(fmt.Errorf("cherry-pick %s onto %s: %w", shortSHA(sha), target, err), repo, checkout)
			}
			return out, &ConflictError{
				Target:    target,
				SHA:       sha,
				Files:     files,
				Remaining: slices.Clone(opts.AfterCurrent[i:]),
				Result:    out,
			}
		}
		out.Picked = append(out.Picked, shortSHA(sha))
	}

	newHEAD, err := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return out, scrubCherryPickErr(err, repo, checkout)
	}
	newHEAD = strings.TrimSpace(newHEAD)
	out.ToSHA = newHEAD
	if newHEAD == "" || newHEAD == out.FromSHA {
		out.Noop = true
		_ = Remove(ctx, repo, checkout, "")
		return out, nil
	}
	dest := "refs/heads/" + target
	if err := runGit(ctx, repo, "push", "origin", newHEAD+":"+dest); err != nil {
		return out, scrubCherryPickErr(fmt.Errorf("push to %s rejected (non-fast-forward or protected): %w", target, err), repo, checkout)
	}
	_ = runGit(ctx, repo, "fetch", "origin", target+":refs/remotes/origin/"+target)
	NoteFetched(repo)
	_ = Remove(ctx, repo, checkout, "")
	return out, nil
}

// AbortCherryPick aborts the sequencer and removes the checkout. Remote unchanged.
func AbortCherryPick(ctx context.Context, repo, checkout string) error {
	repo = strings.TrimSpace(repo)
	checkout = strings.TrimSpace(checkout)
	if checkout != "" && IsRepo(checkout) {
		_ = runGit(ctx, checkout, "cherry-pick", "--abort")
	}
	if repo != "" && checkout != "" {
		return Remove(ctx, repo, checkout, "")
	}
	return nil
}

func leftoverMarkers(ctx context.Context, checkout string, files []string) string {
	for _, f := range files {
		raw, err := ReadWorkingFile(checkout, f, maxConflictFileBytes+1)
		if bytesContainConflictMarker(raw) {
			return f
		}
		if err != nil {
			return f
		}
	}
	return ""
}

func bytesContainConflictMarker(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "<<<<<<<") || strings.Contains(s, ">>>>>>>")
}

// ContainedRelPath joins rel onto checkout and refuses escapes.
func ContainedRelPath(checkout, rel string) (string, error) {
	checkout = strings.TrimSpace(checkout)
	rel = strings.TrimSpace(rel)
	rel = filepath.ToSlash(rel)
	if checkout == "" || rel == "" || rel == "." || strings.Contains(rel, "..") ||
		filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid path")
	}
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	back, err := filepath.Rel(checkout, full)
	if err != nil || strings.HasPrefix(back, "..") || filepath.IsAbs(back) {
		return "", fmt.Errorf("path escapes checkout")
	}
	return full, nil
}

// ReadWorkingFile reads a file under checkout. n-byte cap; larger files return tooBig.
func ReadWorkingFile(checkout, rel string, capBytes int) ([]byte, error) {
	full, err := ContainedRelPath(checkout, rel)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	if capBytes > 0 && len(raw) > capBytes {
		return raw[:capBytes], fmt.Errorf("file too large")
	}
	return raw, nil
}

// WriteWorkingFile writes rel under checkout without git add.
func WriteWorkingFile(checkout, rel string, content []byte) error {
	full, err := ContainedRelPath(checkout, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, content, 0o644)
}

// CheckoutConflictSide writes ours (:2) or theirs (:3) into the working tree
// without staging (so Apply is still required).
func CheckoutConflictSide(ctx context.Context, checkout, rel, side string) error {
	full, err := ContainedRelPath(checkout, rel)
	if err != nil {
		return err
	}
	stage := "2"
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "ours", "2":
		stage = "2"
	case "theirs", "3":
		stage = "3"
	default:
		return fmt.Errorf("side must be ours or theirs")
	}
	raw, err := gitOutputRaw(ctx, checkout, "show", ":"+stage+":"+rel)
	if err != nil {
		return err
	}
	return os.WriteFile(full, raw, 0o644)
}
