package gitworktree

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListOnDisk(t *testing.T) {
	root := t.TempDir()
	if got, err := ListOnDisk(root); err != nil || got != nil {
		t.Fatalf("empty: got %v err %v", got, err)
	}

	mk := func(project, thread string) string {
		p := filepath.Join(root, project, thread)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1 := mk("app", "111")
	p2 := mk("app", "222")
	_ = mk("other", "333")
	// File under worktrees root should be ignored.
	if err := os.WriteFile(filepath.Join(root, "app", "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := ListOnDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len=%d want 3: %+v", len(list), list)
	}
	byPath := map[string]OnDisk{}
	for _, d := range list {
		byPath[d.Path] = d
	}
	if d, ok := byPath[p1]; !ok || d.Project != "app" || d.ThreadID != "111" {
		t.Fatalf("missing p1: %+v", byPath)
	}
	if d, ok := byPath[p2]; !ok || d.ThreadID != "222" {
		t.Fatalf("missing p2: %+v", d)
	}
}

func TestDirModTime(t *testing.T) {
	dir := t.TempDir()
	mt := DirModTime(dir)
	if mt.IsZero() {
		t.Fatal("expected mod time")
	}
	if !DirModTime(filepath.Join(dir, "nope")).IsZero() {
		t.Fatal("missing path should return zero time")
	}
	if time.Since(mt) > time.Minute {
		t.Fatalf("mod time too old: %v", mt)
	}
}

func TestDefaultIdleTTL(t *testing.T) {
	if DefaultIdleTTL != 30*24*time.Hour {
		t.Fatalf("DefaultIdleTTL=%v", DefaultIdleTTL)
	}
}
