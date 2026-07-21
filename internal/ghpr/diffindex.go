package ghpr

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Index / per-file caps for the diff review UI. The index lists every file
// cheaply (no patch content); hunks are fetched per file, so caps apply per
// file instead of per changeset.
const (
	DefaultMaxIndexFiles     = 2000
	DefaultFilePatchBytes    = 400 * 1024
	DefaultFileHunks         = 500
	DefaultMaxIndexScanBytes = 20 * 1024 * 1024 // StatPatch input cap (PR diffs)
)

// FileStat is one changed file from a stat-only pass.
type FileStat struct {
	Path    string // new path (old path for deletions)
	OldPath string // set for renames/copies
	Status  string // single letter: A M D R C T …
	Adds    int
	Dels    int
	Binary  bool
}

// DiffIndex is a changeset file list with totals, no patch content.
type DiffIndex struct {
	Files     []FileStat
	TotalAdds int
	TotalDels int
	Truncated bool // file list capped
}

// FileCaps returns per-file DiffCaps for fragment rendering.
func FileCaps() DiffCaps {
	return DiffCaps{MaxPatchBytes: DefaultFilePatchBytes, MaxFiles: 8, MaxHunks: DefaultFileHunks}
}

// CommitDiffIndexWith lists a commit's changed files via numstat/name-status
// (never generates patch text).
func CommitDiffIndexWith(ctx context.Context, run Runner, cwd, sha string) (DiffIndex, error) {
	if run == nil {
		run = defaultRunner
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return DiffIndex{}, fmt.Errorf("empty commit sha")
	}
	num, err := run(ctx, cwd, "git", "show", "--format=", "--no-ext-diff", "--numstat", "-z", sha)
	if err != nil {
		return DiffIndex{}, err
	}
	st, err := run(ctx, cwd, "git", "show", "--format=", "--no-ext-diff", "--name-status", "-z", sha)
	if err != nil {
		return DiffIndex{}, err
	}
	return buildIndex(num, st), nil
}

// WorktreeDiffIndexWith lists working-tree changes vs the merge-base of
// baseRef and HEAD (same semantics as WorktreeDiffWith).
func WorktreeDiffIndexWith(ctx context.Context, run Runner, cwd, baseRef string) (DiffIndex, error) {
	if run == nil {
		run = defaultRunner
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return DiffIndex{}, fmt.Errorf("empty worktree path")
	}
	left, err := worktreeDiffLeft(ctx, run, cwd, baseRef)
	if err != nil {
		return DiffIndex{}, err
	}
	num, err := run(ctx, cwd, "git", "diff", "--numstat", "-z", left)
	if err != nil {
		return DiffIndex{}, err
	}
	st, err := run(ctx, cwd, "git", "diff", "--name-status", "-z", left)
	if err != nil {
		return DiffIndex{}, err
	}
	return buildIndex(num, st), nil
}

// PRPatchWith fetches the raw unified patch for a PR (gh pr diff has no stat
// or per-file mode — callers slice it with StatPatch / ExtractFilePatch).
func PRPatchWith(ctx context.Context, run Runner, repoDir, selector string) ([]byte, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("empty PR selector")
	}
	return run(ctx, repoDir, "gh", "pr", "diff", selector)
}

func buildIndex(numstat, nameStatus []byte) DiffIndex {
	type stEntry struct {
		letter  string
		oldPath string
	}
	status := map[string]stEntry{}
	toks := strings.Split(string(nameStatus), "\x00")
	for i := 0; i < len(toks); {
		st := toks[i]
		if st == "" {
			i++
			continue
		}
		letter := st[:1]
		if (letter == "R" || letter == "C") && i+2 < len(toks) {
			status[toks[i+2]] = stEntry{letter: letter, oldPath: toks[i+1]}
			i += 3
			continue
		}
		if i+1 < len(toks) {
			status[toks[i+1]] = stEntry{letter: letter}
		}
		i += 2
	}

	var d DiffIndex
	toks = strings.Split(string(numstat), "\x00")
	for i := 0; i < len(toks); {
		t := toks[i]
		if t == "" {
			i++
			continue
		}
		parts := strings.SplitN(t, "\t", 3)
		if len(parts) < 3 {
			i++
			continue
		}
		f := FileStat{}
		f.Adds, f.Binary = parseNumstatCount(parts[0])
		var binDel bool
		f.Dels, binDel = parseNumstatCount(parts[1])
		f.Binary = f.Binary || binDel
		if parts[2] == "" {
			// Rename/copy: "adds\tdels\t" NUL old NUL new.
			if i+2 >= len(toks) {
				break
			}
			f.OldPath = toks[i+1]
			f.Path = toks[i+2]
			i += 3
		} else {
			f.Path = parts[2]
			i++
		}
		if st, ok := status[f.Path]; ok {
			f.Status = st.letter
			if f.OldPath == "" {
				f.OldPath = st.oldPath
			}
		} else {
			f.Status = "M"
		}
		if len(d.Files) >= DefaultMaxIndexFiles {
			d.Truncated = true
			break
		}
		d.TotalAdds += f.Adds
		d.TotalDels += f.Dels
		d.Files = append(d.Files, f)
	}
	return d
}

func parseNumstatCount(s string) (n int, binary bool) {
	if s == "-" {
		return 0, true
	}
	n, _ = strconv.Atoi(s)
	return n, false
}

// StatPatch scans a unified patch into a DiffIndex without keeping lines.
func StatPatch(patch []byte, maxFiles int) DiffIndex {
	if maxFiles <= 0 {
		maxFiles = DefaultMaxIndexFiles
	}
	var d DiffIndex
	if len(patch) > DefaultMaxIndexScanBytes {
		patch = patch[:DefaultMaxIndexScanBytes]
		d.Truncated = true
	}
	text := string(bytes.ReplaceAll(patch, []byte("\r\n"), []byte("\n")))
	var cur *FileStat
	inHunk := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if len(d.Files) >= maxFiles {
				d.Truncated = true
				return d
			}
			oldP, newP := parseDiffGitHeader(line)
			d.Files = append(d.Files, FileStat{Path: newP, OldPath: oldP, Status: "M"})
			cur = &d.Files[len(d.Files)-1]
			inHunk = false
		case cur == nil:
			// preamble before first file
		case inHunk && strings.HasPrefix(line, "+"):
			cur.Adds++
			d.TotalAdds++
		case inHunk && strings.HasPrefix(line, "-"):
			cur.Dels++
			d.TotalDels++
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "A"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "D"
		case strings.HasPrefix(line, "rename from "):
			cur.Status = "R"
			cur.OldPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
			cur.Binary = true
		case strings.HasPrefix(line, "--- "):
			if p := strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/"); p != "/dev/null" {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p := strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/"); p != "/dev/null" {
				cur.Path = p
			}
		}
	}
	// Normalize: OldPath only meaningful for renames/copies.
	for i := range d.Files {
		f := &d.Files[i]
		if f.Status != "R" && f.Status != "C" {
			if f.Path == "" {
				f.Path = f.OldPath
			}
			f.OldPath = ""
		}
	}
	return d
}

// filePathspec returns git pathspec args for one file (old path included so
// rename hunks survive pathspec limiting).
func filePathspec(path, oldPath string) []string {
	args := []string{"--", path}
	if oldPath != "" && oldPath != path {
		args = append(args, oldPath)
	}
	return args
}

// ShowCommitFileWith returns one file's parsed diff from a commit.
func ShowCommitFileWith(ctx context.Context, run Runner, cwd, sha, path, oldPath string, caps DiffCaps) (Diff, error) {
	if run == nil {
		run = defaultRunner
	}
	sha = strings.TrimSpace(sha)
	if sha == "" || strings.TrimSpace(path) == "" {
		return Diff{}, fmt.Errorf("empty sha or path")
	}
	args := append([]string{"show", "--format=", "-p", "--no-ext-diff", sha}, filePathspec(path, oldPath)...)
	raw, err := run(ctx, cwd, "git", args...)
	if err != nil {
		return Diff{}, err
	}
	return ParseUnifiedDiff(raw, caps), nil
}

// WorktreeDiffFileWith returns one file's working-tree diff vs the merge-base
// of baseRef and HEAD (same semantics as WorktreeDiffWith).
func WorktreeDiffFileWith(ctx context.Context, run Runner, cwd, baseRef, path, oldPath string, caps DiffCaps) (Diff, error) {
	if run == nil {
		run = defaultRunner
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return Diff{}, fmt.Errorf("empty worktree path")
	}
	if strings.TrimSpace(path) == "" {
		return Diff{}, fmt.Errorf("empty path")
	}
	left, err := worktreeDiffLeft(ctx, run, cwd, baseRef)
	if err != nil {
		return Diff{}, err
	}
	args := append([]string{"diff", left}, filePathspec(path, oldPath)...)
	raw, err := run(ctx, cwd, "git", args...)
	if err != nil {
		return Diff{}, err
	}
	return ParseUnifiedDiff(raw, caps), nil
}

// ExtractFilePatch returns the "diff --git" section for path (matched against
// either side), or nil when absent.
func ExtractFilePatch(patch []byte, path string) []byte {
	text := bytes.ReplaceAll(patch, []byte("\r\n"), []byte("\n"))
	const marker = "diff --git "
	start := -1
	lines := bytes.Split(text, []byte("\n"))
	offset := 0
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte(marker)) {
			if start >= 0 {
				return text[start:offset]
			}
			oldP, newP := parseDiffGitHeader(string(line))
			if oldP == path || newP == path {
				start = offset
			}
		}
		offset += len(line) + 1
	}
	if start >= 0 {
		end := len(text)
		return text[start:end]
	}
	return nil
}
