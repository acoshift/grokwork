package gitworktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointRefRoundTrip(t *testing.T) {
	ref := CheckpointRef("123456789012345678", "cp01")
	want := "refs/grok-cp/123456789012345678/cp01"
	if ref != want {
		t.Fatalf("ref=%q want %q", ref, want)
	}
	tid, id, ok := ParseCheckpointRef(ref)
	if !ok || tid != "123456789012345678" || id != "cp01" {
		t.Fatalf("parse: %q %q %v", tid, id, ok)
	}
	if !IsCheckpointRefForThread(ref, "123456789012345678") {
		t.Fatal("expected match")
	}
	if IsCheckpointRefForThread(ref, "other") {
		t.Fatal("cross-thread must fail")
	}
	if _, _, ok := ParseCheckpointRef("refs/heads/main"); ok {
		t.Fatal("branch ref not a checkpoint")
	}
}

func TestCreateCheckpointAndReset(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init")
	runGitTest(t, repo, "config", "user.email", "t@t")
	runGitTest(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "a.txt")
	runGitTest(t, repo, "commit", "-m", "v1")
	runGitTest(t, repo, "branch", "-M", "main")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	sha1, err := HeadSHA(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	got, ref, err := CreateCheckpointRef(ctx, repo, "thr1", "c1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != sha1 {
		t.Fatalf("sha=%s want %s", got, sha1)
	}
	if !IsCheckpointRefForThread(ref, "thr1") {
		t.Fatalf("ref=%s", ref)
	}

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "a.txt")
	runGitTest(t, repo, "commit", "-m", "v2")

	if err := HardResetToSHA(ctx, repo, sha1); err != nil {
		t.Fatal(err)
	}
	head, err := HeadSHA(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha1 {
		t.Fatalf("after reset head=%s want %s", head, sha1)
	}
	body, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" {
		t.Fatalf("content=%q", body)
	}
}
