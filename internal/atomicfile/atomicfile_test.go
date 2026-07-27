package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFileWithPermAndNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	if err := Write(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("content = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}

	// A shorter second write must fully replace the first, not overlay it.
	if err := Write(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{}` {
		t.Fatalf("content after replace = %q, want {}", got)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

// The whole reason for the temp file: a write that cannot complete must leave
// the previous contents intact rather than a truncated, unparseable file.
func TestFailedWriteLeavesPreviousContentAndNoTemp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not block root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := Write(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := Write(path, []byte("replacement"), 0o600); err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("content = %q, want the original preserved", got)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after a failed write: %v", matches)
	}
}

func TestWriteFailsWhenDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "store.json")
	if err := Write(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}
