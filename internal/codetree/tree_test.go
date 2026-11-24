package codetree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanPathRejectsEscape(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", `foo\bar`, "foo/../../x", "..", "a/\x00b"} {
		if _, err := CleanPath(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("CleanPath(%q) err=%v", p, err)
		}
	}
	got, err := CleanPath("a/b/../c")
	if err != nil || got != "a/c" {
		t.Fatalf("clean=%q err=%v", got, err)
	}
	got, err = CleanPath("")
	if err != nil || got != "" {
		t.Fatalf("root=%q err=%v", got, err)
	}
}

func TestParseTreeSubmoduleSize(t *testing.T) {
	oid := strings.Repeat("a", 40)
	raw := []byte("160000 commit " + oid + " -\tlib\x00")
	entries, err := ParseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Size != -1 || entries[0].Kind() != KindSubmodule || entries[0].Name != "lib" {
		t.Fatalf("%+v", entries)
	}
}

func TestParseTreeSortsDirsFirst(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)
	raw := []byte(
		"100644 blob " + a + " 1\tB.txt\x00" +
			"040000 tree " + b + " -\tzed\x00" +
			"100644 blob " + c + " 1\ta.txt\x00",
	)
	entries, err := ParseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	Sort(entries)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	got := strings.Join(names, ",")
	if got != "zed,a.txt,B.txt" {
		t.Fatalf("order %s", got)
	}
}

func TestReadDirectoryListsTreeOID(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	blob := strings.Repeat("c", 40)
	var calls []string
	run := func(_ context.Context, _ string, maxOut int64, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		joined := strings.Join(args, " ")
		if strings.Contains(joined, commit) {
			return []byte("040000 tree " + tree + " -\tdir\x00"), nil
		}
		if strings.Contains(joined, " -- ") {
			t.Fatalf("child listing took a path: %s", joined)
		}
		if !strings.Contains(joined, tree) {
			t.Fatalf("second ls-tree oid: %s", joined)
		}
		if maxOut <= 0 {
			t.Fatal("listing has no cap")
		}
		return []byte("100644 blob " + blob + " 2\thi\x00"), nil
	}
	node, err := Read(t.Context(), run, "/repo", commit, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Dir || len(node.Children) != 1 || node.Children[0].Name != "hi" {
		t.Fatalf("%+v", node)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], "--literal-pathspecs") {
		t.Fatalf("calls=%v", calls)
	}
}

func TestReadBlobSkipsOverCap(t *testing.T) {
	var calls int
	run := func(context.Context, string, int64, ...string) ([]byte, error) {
		calls++
		return []byte("nope"), nil
	}
	_, err := ReadBlob(t.Context(), run, "/repo", strings.Repeat("d", 40), MaxBlobBytes+1, MaxBlobBytes)
	if !errors.Is(err, ErrTooBig) {
		t.Fatalf("err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("cat-file calls=%d", calls)
	}
}

func TestReadBlobKillsOverflow(t *testing.T) {
	dir := initRepo(t)
	body := bytes.Repeat([]byte("x"), 64)
	if err := os.WriteFile(filepath.Join(dir, "big"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "big")
	git(t, dir, "commit", "-m", "big")
	oid := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD:big"))
	// Advertised size is under the cap. The object is not.
	got, err := ReadBlob(t.Context(), nil, dir, oid, 8, 8)
	if !errors.Is(err, ErrTooBig) {
		t.Fatalf("err=%v body=%q", err, got)
	}
}

func TestSymlinkIsTextNotFollowed(t *testing.T) {
	dir := initRepo(t)
	if err := os.Symlink("../outside", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "link")
	git(t, dir, "commit", "-m", "link")
	commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	node, err := Read(t.Context(), nil, dir, commit, "link")
	if err != nil {
		t.Fatal(err)
	}
	if !node.Symlink {
		t.Fatalf("%+v", node)
	}
	body, err := ReadBlob(t.Context(), nil, dir, node.Entry.OID, node.Entry.Size, MaxBlobBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "../outside" {
		t.Fatalf("body=%q", body)
	}
}

func TestLiteralStarFile(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "*"), []byte("star\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "star")
	commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	node, err := Read(t.Context(), nil, dir, commit, "*")
	if err != nil {
		t.Fatal(err)
	}
	if !node.File || node.Entry.Name != "*" {
		t.Fatalf("%+v", node)
	}
	root, err := Read(t.Context(), nil, dir, commit, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range root.Children {
		names = append(names, e.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "*") {
		t.Fatalf("root=%v", names)
	}
}

func TestResolveCommitRejectsFlag(t *testing.T) {
	var calls int
	run := func(context.Context, string, int64, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("should not run")
	}
	for _, rev := range []string{"-upload-pack=evil", "", "refs/heads/main", "origin/main"} {
		_, err := ResolveCommit(t.Context(), run, "/repo", rev)
		if !errors.Is(err, ErrBadRev) {
			t.Errorf("rev %q err=%v", rev, err)
		}
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestBinaryNULAndUTF8(t *testing.T) {
	if !Binary([]byte{'a', 0, 'b'}) {
		t.Fatal("NUL")
	}
	if !Binary([]byte{0xff, 0xfe}) {
		t.Fatal("utf-8")
	}
	if Binary([]byte("plain\n")) {
		t.Fatal("text")
	}
	if !LFSPointer([]byte("version https://git-lfs.github.com/spec/v1\noid sha256:abc\n")) {
		t.Fatal("lfs")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README")
	git(t, dir, "commit", "-m", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
