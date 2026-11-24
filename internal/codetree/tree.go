// Package codetree reads a git tree by object id.
//
// The project checkout is shared and dirty, and it is not checked out at the
// revision on screen, so this package never reads the work tree. Callers pass
// a commit or tree oid they already resolved. Paths are literal. Blob bodies
// are refused before git runs when the tree says they exceed the cap, and the
// runner kills git if stdout still grows past the cap.
package codetree

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxBlobBytes is the most a file body may occupy, inline or downloaded.
	MaxBlobBytes = 1 << 20
	// maxTreeOutput bounds one ls-tree. A parsed partial record would be a lie.
	maxTreeOutput = 8 << 20
	// binarySniff is how much of a blob is checked for a NUL before the rest
	// is treated as text. The whole blob is still checked for valid UTF-8.
	binarySniff = 8 << 10
)

var (
	ErrNotFound     = errors.New("path not found")
	ErrTooBig       = errors.New("blob exceeds size cap")
	ErrOutputCapped = errors.New("git output exceeded cap")
	ErrBadPath      = errors.New("invalid path")
	ErrBadRev       = errors.New("invalid revision")
	ErrNotFile      = errors.New("not a file")
)

// Kind is how a tree entry is shown.
type Kind int

const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	KindSubmodule
)

// Entry is one ls-tree -l -z record.
type Entry struct {
	Mode string
	Type string
	OID  string
	// Size is the blob size. Trees and submodules report -1 (git prints "-").
	Size int64
	Path string
	Name string
}

// Kind classifies the entry. Mode wins for symlinks and submodules because
// git still reports those as blob and commit.
func (e Entry) Kind() Kind {
	switch e.Mode {
	case "120000":
		return KindSymlink
	case "160000":
		return KindSubmodule
	}
	switch e.Type {
	case "tree":
		return KindDir
	case "commit":
		return KindSubmodule
	default:
		return KindFile
	}
}

// Node is a path opened at one commit: a directory's children, or one entry.
type Node struct {
	Entry     Entry
	Children  []Entry
	Dir       bool
	File      bool
	Symlink   bool
	Submodule bool
}

// Runner runs git in dir and returns at most maxOut bytes of stdout.
// More than maxOut is ErrOutputCapped. The production runner kills git.
type Runner func(ctx context.Context, dir string, maxOut int64, args ...string) ([]byte, error)

// Run is the production runner. maxOut is a hard stdout cap.
func Run(ctx context.Context, dir string, maxOut int64, args ...string) ([]byte, error) {
	if maxOut < 0 {
		return nil, fmt.Errorf("negative output cap")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	buf, readErr := io.ReadAll(io.LimitReader(stdout, maxOut+1))
	if readErr != nil {
		killWait(cmd)
		return nil, readErr
	}
	if int64(len(buf)) > maxOut {
		killWait(cmd)
		return nil, ErrOutputCapped
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return buf, nil
}

func killWait(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

// CleanPath is the path a tree lookup will accept. Empty is the root.
// A cleaned ".." is rejected. Separators are slashes.
func CleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return "", nil
	}
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrBadPath
	}
	return clean, nil
}

// FullOID reports whether s is a Git SHA-1 or SHA-256 object name.
func FullOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if r < '0' || (r > '9' && r < 'a') || r > 'f' {
			return false
		}
	}
	return true
}

// ResolveCommit peels rev to a full commit oid.
// rev must be HEAD, a refs/remotes/origin or refs/tags name, or hex.
// The returned oid is git's, not the input.
func ResolveCommit(ctx context.Context, run Runner, repo, rev string) (string, error) {
	if run == nil {
		run = Run
	}
	rev = strings.TrimSpace(rev)
	if !allowedRev(rev) {
		return "", ErrBadRev
	}
	out, err := run(ctx, repo, 80, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if !FullOID(oid) {
		return "", fmt.Errorf("rev-parse: unexpected oid %q", oid)
	}
	return oid, nil
}

func allowedRev(rev string) bool {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return false
	}
	if rev == "HEAD" {
		return true
	}
	if isHex(rev) && len(rev) >= 7 && len(rev) <= 64 {
		return true
	}
	var rest string
	switch {
	case strings.HasPrefix(rev, "refs/remotes/origin/"):
		rest = strings.TrimPrefix(rev, "refs/remotes/origin/")
	case strings.HasPrefix(rev, "refs/tags/"):
		rest = strings.TrimPrefix(rev, "refs/tags/")
	default:
		return false
	}
	if rest == "" || rest == "." || rest == ".." {
		return false
	}
	if strings.Contains(rest, "..") || strings.ContainsAny(rest, " \t\n\r^:~\\?[*") {
		return false
	}
	return true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || (r > '9' && r < 'a') || r > 'f' {
			return false
		}
	}
	return true
}

// TagRefs lists refs/tags/* by full refname.
func TagRefs(ctx context.Context, run Runner, repo string) ([]string, error) {
	if run == nil {
		run = Run
	}
	out, err := run(ctx, repo, maxTreeOutput, "for-each-ref", "--format=%(refname)", "refs/tags")
	if err != nil {
		return nil, err
	}
	var refs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "refs/tags/") {
			continue
		}
		refs = append(refs, line)
	}
	return refs, nil
}

// ParseTree parses git ls-tree -z -l output.
func ParseTree(raw []byte) ([]Entry, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []Entry
	for rec := range bytes.SplitSeq(raw, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, rel, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("ls-tree: missing tab")
		}
		fields := bytes.Fields(meta)
		if len(fields) != 4 {
			return nil, fmt.Errorf("ls-tree: bad record %q", meta)
		}
		e := Entry{
			Mode: string(fields[0]),
			Type: string(fields[1]),
			OID:  string(fields[2]),
			Path: string(rel),
		}
		if !FullOID(e.OID) {
			return nil, fmt.Errorf("ls-tree: bad oid %q", e.OID)
		}
		if bytes.Equal(fields[3], []byte("-")) {
			e.Size = -1
		} else {
			n, err := strconv.ParseInt(string(fields[3]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("ls-tree: size: %w", err)
			}
			e.Size = n
		}
		e.Name = path.Base(e.Path)
		if e.Name == "." || e.Name == ".." || e.Name == "" || strings.Contains(e.Name, "/") {
			return nil, fmt.Errorf("ls-tree: bad name %q", e.Path)
		}
		out = append(out, e)
	}
	return out, nil
}

// Sort orders directories, then files and symlinks, then submodules.
// Names compare case-insensitively, then by bytes.
func Sort(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		if c := cmp.Compare(kindRank(a.Kind()), kindRank(b.Kind())); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

func kindRank(k Kind) int {
	switch k {
	case KindDir:
		return 0
	case KindSubmodule:
		return 2
	default:
		return 1
	}
}

// Read opens path at commitOID. An empty path lists the root.
// A directory is a second ls-tree of that tree oid, with no path argument.
func Read(ctx context.Context, run Runner, repo, commitOID, requestPath string) (Node, error) {
	if run == nil {
		run = Run
	}
	if !FullOID(commitOID) {
		return Node{}, ErrBadRev
	}
	clean, err := CleanPath(requestPath)
	if err != nil {
		return Node{}, err
	}
	if clean == "" {
		kids, err := list(ctx, run, repo, commitOID)
		if err != nil {
			return Node{}, err
		}
		return Node{Dir: true, Children: kids}, nil
	}
	ent, err := lookup(ctx, run, repo, commitOID, clean)
	if err != nil {
		return Node{}, err
	}
	switch ent.Kind() {
	case KindDir:
		kids, err := list(ctx, run, repo, ent.OID)
		if err != nil {
			return Node{}, err
		}
		return Node{Entry: ent, Dir: true, Children: kids}, nil
	case KindSymlink:
		return Node{Entry: ent, Symlink: true}, nil
	case KindSubmodule:
		return Node{Entry: ent, Submodule: true}, nil
	default:
		return Node{Entry: ent, File: true}, nil
	}
}

func lookup(ctx context.Context, run Runner, repo, commitOID, clean string) (Entry, error) {
	out, err := run(ctx, repo, maxTreeOutput,
		"--literal-pathspecs", "ls-tree", "-z", "-l", "--full-tree", commitOID, "--", clean)
	if err != nil {
		return Entry{}, err
	}
	entries, err := ParseTree(out)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) == 0 {
		return Entry{}, ErrNotFound
	}
	if len(entries) != 1 {
		return Entry{}, fmt.Errorf("ls-tree: %d entries for one path", len(entries))
	}
	if entries[0].Path != clean && path.Clean(entries[0].Path) != clean {
		return Entry{}, fmt.Errorf("ls-tree: path %q does not match %q", entries[0].Path, clean)
	}
	return entries[0], nil
}

// ListPaths returns every path in commitOID's tree. It reads that object,
// not the work tree. commitOID must already be a full oid. A path git
// prints that CleanPath would reject is dropped. Output over the tree cap
// is an error, not a partial list.
func ListPaths(ctx context.Context, run Runner, repo, commitOID string) ([]string, error) {
	if run == nil {
		run = Run
	}
	if !FullOID(commitOID) {
		return nil, ErrBadRev
	}
	out, err := run(ctx, repo, maxTreeOutput,
		"--literal-pathspecs", "ls-tree", "-r", "-z", "--name-only", commitOID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		clean, err := CleanPath(string(rec))
		if err != nil || clean == "" {
			continue
		}
		paths = append(paths, clean)
	}
	return paths, nil
}

func list(ctx context.Context, run Runner, repo, treeOID string) ([]Entry, error) {
	if !FullOID(treeOID) {
		return nil, ErrBadRev
	}
	out, err := run(ctx, repo, maxTreeOutput,
		"--literal-pathspecs", "ls-tree", "-z", "-l", treeOID)
	if err != nil {
		return nil, err
	}
	entries, err := ParseTree(out)
	if err != nil {
		return nil, err
	}
	Sort(entries)
	return entries, nil
}

// ReadBlob returns the blob. size is the ls-tree size. A size over cap does
// not start git. The runner's cap is the second line if the object is larger
// than advertised.
func ReadBlob(ctx context.Context, run Runner, repo, oid string, size, cap int64) ([]byte, error) {
	if run == nil {
		run = Run
	}
	if !FullOID(oid) {
		return nil, ErrBadRev
	}
	if size < 0 {
		return nil, ErrNotFile
	}
	if cap < 0 {
		cap = MaxBlobBytes
	}
	if size > cap {
		return nil, ErrTooBig
	}
	out, err := run(ctx, repo, cap+1, "cat-file", "blob", oid)
	if errors.Is(err, ErrOutputCapped) {
		return nil, ErrTooBig
	}
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > cap {
		return nil, ErrTooBig
	}
	return out, nil
}

// Binary reports whether a blob should not be shown as text.
// A NUL in the first 8 KiB, or any invalid UTF-8, is binary.
func Binary(b []byte) bool {
	n := min(len(b), binarySniff)
	if bytes.IndexByte(b[:n], 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}

// LFSPointer reports a Git LFS pointer payload. cat-file does not smudge.
func LFSPointer(b []byte) bool {
	return bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec"))
}
