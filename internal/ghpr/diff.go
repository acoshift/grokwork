package ghpr

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Default diff caps for browser-safe payloads.
const (
	DefaultMaxPatchBytes = 200 * 1024
	DefaultMaxFiles      = 50
	DefaultMaxHunks      = 200
)

// DiffCaps limits ParseUnifiedDiff output size.
type DiffCaps struct {
	MaxPatchBytes int // input truncate; 0 → DefaultMaxPatchBytes
	MaxFiles      int // 0 → DefaultMaxFiles
	MaxHunks      int // total hunks across files; 0 → DefaultMaxHunks
}

func (c DiffCaps) normalized() DiffCaps {
	if c.MaxPatchBytes <= 0 {
		c.MaxPatchBytes = DefaultMaxPatchBytes
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = DefaultMaxFiles
	}
	if c.MaxHunks <= 0 {
		c.MaxHunks = DefaultMaxHunks
	}
	return c
}

// DiffOpts controls how PR diffs are fetched.
type DiffOpts struct {
	// NameOnly requests only the file list (gh pr diff --name-only).
	NameOnly bool
	Caps     DiffCaps
}

// Hunk is one unified-diff hunk.
type Hunk struct {
	Header string   // @@ -a,b +c,d @@ …
	Lines  []string // including leading ' ', '+', '-', '\'
}

// DiffFile is one file in a unified diff.
type DiffFile struct {
	PathOld string
	PathNew string
	Hunks   []Hunk
}

// Diff is a structured unified patch.
type Diff struct {
	Files     []DiffFile
	Truncated bool
	RawBytes  int // original patch size before caps
}

// PRDiff fetches `gh pr diff` and parses it.
func PRDiff(ctx context.Context, repoDir, selector string, opts DiffOpts) (Diff, error) {
	return PRDiffWith(ctx, defaultRunner, repoDir, selector, opts)
}

// PRDiffWith is PRDiff with an injectable runner.
func PRDiffWith(ctx context.Context, run Runner, repoDir, selector string, opts DiffOpts) (Diff, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Diff{}, fmt.Errorf("empty PR selector")
	}
	args := []string{"pr", "diff", selector}
	if opts.NameOnly {
		args = append(args, "--name-only")
	}
	raw, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return Diff{}, err
	}
	if opts.NameOnly {
		return parseNameOnlyDiff(raw, opts.Caps.normalized()), nil
	}
	return ParseUnifiedDiff(raw, opts.Caps), nil
}

// WorktreeDiff runs a working-tree diff in cwd against the merge-base of
// baseRef and HEAD (so it matches PR-style branch changes, not "everything
// missing from tip of baseRef").
func WorktreeDiff(ctx context.Context, cwd, baseRef string) (Diff, error) {
	return WorktreeDiffWith(ctx, defaultRunner, cwd, baseRef, DiffCaps{})
}

// WorktreeDiffWith is WorktreeDiff with injectable runner and caps.
func WorktreeDiffWith(ctx context.Context, run Runner, cwd, baseRef string, caps DiffCaps) (Diff, error) {
	if run == nil {
		run = defaultRunner
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		// Empty Dir would make the process cwd the git root (often the bot repo).
		return Diff{}, fmt.Errorf("empty worktree path")
	}
	left, err := worktreeDiffLeft(ctx, run, cwd, baseRef)
	if err != nil {
		return Diff{}, err
	}
	// Working tree (committed branch tip + staged + unstaged) vs merge-base.
	// Includes uncommitted changes; does not invent deletions for files only
	// added on baseRef after the branch point (unlike two-dot `git diff baseRef`).
	raw, err := run(ctx, cwd, "git", "diff", left)
	if err != nil {
		return Diff{}, err
	}
	return ParseUnifiedDiff(raw, caps), nil
}

// worktreeDiffLeft picks the left side of a worktree diff.
// baseRef empty or HEAD → HEAD (uncommitted only).
// Otherwise → merge-base(baseRef, HEAD), i.e. three-dot fork point, so the
// working tree can still be the right side (unlike baseRef...HEAD which is
// commit trees only).
func worktreeDiffLeft(ctx context.Context, run Runner, cwd, baseRef string) (string, error) {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" || baseRef == "HEAD" {
		return "HEAD", nil
	}
	out, err := run(ctx, cwd, "git", "merge-base", baseRef, "HEAD")
	if err != nil {
		return "", fmt.Errorf("merge-base %s HEAD: %w", baseRef, err)
	}
	mb := strings.TrimSpace(string(out))
	if mb == "" {
		return "", fmt.Errorf("empty merge-base for %s and HEAD", baseRef)
	}
	return mb, nil
}

func parseNameOnlyDiff(raw []byte, caps DiffCaps) Diff {
	d := Diff{RawBytes: len(raw)}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(d.Files) >= caps.MaxFiles {
			d.Truncated = true
			break
		}
		d.Files = append(d.Files, DiffFile{PathNew: line, PathOld: line})
	}
	return d
}

// ParseUnifiedDiff turns a unified patch into Diff with caps (never panics).
func ParseUnifiedDiff(patch []byte, caps DiffCaps) Diff {
	caps = caps.normalized()
	d := Diff{RawBytes: len(patch)}
	if len(patch) == 0 {
		return d
	}
	if len(patch) > caps.MaxPatchBytes {
		patch = patch[:caps.MaxPatchBytes]
		d.Truncated = true
	}

	// Normalize newlines.
	text := string(bytes.ReplaceAll(patch, []byte("\r\n"), []byte("\n")))
	lines := strings.Split(text, "\n")

	var cur *DiffFile
	hunkCount := 0
	i := 0
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if len(d.Files) >= caps.MaxFiles {
				d.Truncated = true
				return d
			}
			oldP, newP := parseDiffGitHeader(line)
			d.Files = append(d.Files, DiffFile{PathOld: oldP, PathNew: newP})
			cur = &d.Files[len(d.Files)-1]
			i++
		case strings.HasPrefix(line, "--- "):
			if cur != nil {
				p := strings.TrimPrefix(line, "--- ")
				p = strings.TrimPrefix(p, "a/")
				if p != "/dev/null" {
					cur.PathOld = p
				}
			}
			i++
		case strings.HasPrefix(line, "+++ "):
			if cur != nil {
				p := strings.TrimPrefix(line, "+++ ")
				p = strings.TrimPrefix(p, "b/")
				if p != "/dev/null" {
					cur.PathNew = p
				}
			}
			i++
		case strings.HasPrefix(line, "@@"):
			if cur == nil {
				// orphan hunk — start anonymous file
				if len(d.Files) >= caps.MaxFiles {
					d.Truncated = true
					return d
				}
				d.Files = append(d.Files, DiffFile{})
				cur = &d.Files[len(d.Files)-1]
			}
			if hunkCount >= caps.MaxHunks {
				d.Truncated = true
				return d
			}
			h := Hunk{Header: line}
			i++
			for i < len(lines) {
				l := lines[i]
				if strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "@@") {
					break
				}
				// file headers mid-stream
				if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
					break
				}
				h.Lines = append(h.Lines, l)
				i++
			}
			cur.Hunks = append(cur.Hunks, h)
			hunkCount++
		default:
			i++
		}
	}
	return d
}

func parseDiffGitHeader(line string) (oldP, newP string) {
	// diff --git a/foo b/foo
	rest := strings.TrimPrefix(line, "diff --git ")
	parts := strings.Fields(rest)
	if len(parts) >= 2 {
		oldP = strings.TrimPrefix(parts[0], "a/")
		newP = strings.TrimPrefix(parts[1], "b/")
	}
	return oldP, newP
}

// FilePaths returns new paths (or old if new empty) for listing.
func (d Diff) FilePaths() []string {
	out := make([]string, 0, len(d.Files))
	for _, f := range d.Files {
		p := f.PathNew
		if p == "" {
			p = f.PathOld
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HunkCount returns total hunks.
func (d Diff) HunkCount() int {
	n := 0
	for _, f := range d.Files {
		n += len(f.Hunks)
	}
	return n
}

// FormatStat returns a short "N files" summary.
func (d Diff) FormatStat() string {
	return strconv.Itoa(len(d.Files)) + " file(s), " + strconv.Itoa(d.HunkCount()) + " hunk(s)"
}
