package gitworktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePrimaryBranch(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	// Set default branch to main if git used master.
	_ = exec.Command("git", "-C", repo, "branch", "-M", "main").Run()
	name, remote, err := ResolvePrimaryBranch(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("empty name")
	}
	// Without origin, remote may still be origin/<name>
	if !strings.Contains(remote, name) {
		t.Fatalf("remote=%q name=%q", remote, name)
	}
}

func TestDirectShipFFRefusesCreateWhenPrimaryArgMissingOnOrigin(t *testing.T) {
	ctx := t.Context()
	_, main, worktree, branch := setupDirectShipFixture(t)
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "f.txt")
	runGitTest(t, worktree, "commit", "-m", "f")

	// Concrete primary that does not exist on origin — must refuse (no create-on-push).
	_, err := DirectShipFF(ctx, main, worktree, branch, "no-such-primary")
	if err == nil {
		t.Fatal("expected refuse when origin/no-such-primary missing")
	}
	if !strings.Contains(err.Error(), "no-such-primary") {
		t.Fatalf("error: %v", err)
	}
	// Branch must not have been created on remote.
	if out, err := gitOutput(ctx, main, "ls-remote", "--heads", "origin", "no-such-primary"); err == nil {
		if strings.TrimSpace(out) != "" {
			t.Fatalf("remote branch was created: %q", out)
		}
	}
}

func TestDirectShipFFSuccessAndNoop(t *testing.T) {
	ctx := context.Background()
	remote, main, worktree, branch := setupDirectShipFixture(t)

	// Commit on session branch.
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "feature.txt")
	runGitTest(t, worktree, "commit", "-m", "feature")

	head, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)

	res, err := DirectShipFF(ctx, main, worktree, branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA != head || res.PrimaryBranch != "main" {
		t.Fatalf("res=%+v", res)
	}

	// Remote main should match.
	remoteMain, err := gitOutput(ctx, remote, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(remoteMain) != head {
		t.Fatalf("remote main=%s want %s", remoteMain, head)
	}

	// Second ship is noop.
	res2, err := DirectShipFF(ctx, main, worktree, branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Noop {
		t.Fatalf("want noop, got %+v", res2)
	}
}

func TestDirectShipFFRejectsNonManagedAndDirty(t *testing.T) {
	ctx := context.Background()
	_, main, worktree, branch := setupDirectShipFixture(t)

	if _, err := DirectShipFF(ctx, main, worktree, "feature/not-managed", "main"); err == nil {
		t.Fatal("want refuse non-managed")
	}

	if err := os.WriteFile(filepath.Join(worktree, "README"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DirectShipFF(ctx, main, worktree, branch, "main"); err == nil {
		t.Fatal("want dirty tracked fail")
	}
	// Restore and leave only untracked — should still ship (noop or success).
	runGitTest(t, worktree, "checkout", "--", "README")
	if err := os.WriteFile(filepath.Join(worktree, "scratch.tmp"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DirectShipFF(ctx, main, worktree, branch, "main"); err != nil {
		t.Fatalf("untracked should not block: %v", err)
	}
}

func TestDirectShipFFNonFastForward(t *testing.T) {
	ctx := context.Background()
	remote, main, worktree, branch := setupDirectShipFixture(t)

	// Session commit.
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "a.txt")
	runGitTest(t, worktree, "commit", "-m", "a")

	// Divergent commit on remote main (not ancestor of session).
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", remote, cloneDir)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runGitTest(t, cloneDir, "checkout", "-B", "main", "origin/main")
	if err := os.WriteFile(filepath.Join(cloneDir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, cloneDir, "add", "b.txt")
	runGitTest(t, cloneDir, "commit", "-m", "b")
	runGitTest(t, cloneDir, "push", "origin", "HEAD:main")

	// Fetch so main sees new origin/main; session is now non-ff.
	runGitTest(t, main, "fetch", "origin")
	_, err := DirectShipFF(ctx, main, worktree, branch, "main")
	if err == nil {
		t.Fatal("want non-ff error")
	}
	if !strings.Contains(err.Error(), "non-fast-forward") && !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("unexpected err: %v", err)
	}
	if !IsNonFastForward(err) {
		t.Fatalf("IsNonFastForward(%v)=false", err)
	}
}

func TestIsNonFastForward(t *testing.T) {
	if IsNonFastForward(nil) {
		t.Fatal("nil")
	}
	if IsNonFastForward(fmt.Errorf("push to main rejected (non-fast-forward or protected): hook declined")) {
		t.Fatal("wrapper text alone must not match")
	}
	if IsNonFastForward(fmt.Errorf("worktree has uncommitted changes to tracked files")) {
		t.Fatal("dirty is not non-ff")
	}
	if !IsNonFastForward(fmt.Errorf("non-fast-forward: origin/main is not an ancestor of session HEAD (primary may have advanced)")) {
		t.Fatal("pre-check")
	}
	if !IsNonFastForward(fmt.Errorf("push to main rejected (non-fast-forward or protected): ! [rejected] abc -> main (non-fast-forward)")) {
		t.Fatal("git rejected")
	}
}

func TestDirectShipFFCatchUpMergesThenShips(t *testing.T) {
	ctx := t.Context()
	remote, main, worktree, branch := setupDirectShipFixture(t)

	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "a.txt")
	runGitTest(t, worktree, "commit", "-m", "a")

	// Divergent commit on remote main (different file — merge is clean).
	advanceRemoteMain(t, remote, "b.txt", "b\n", "b")

	res, err := DirectShipFFCatchUp(ctx, main, worktree, branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.PrimaryBranch != "main" {
		t.Fatalf("res=%+v", res)
	}

	remoteMain, err := gitOutput(ctx, remote, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(remoteMain) != res.ToSHA {
		t.Fatalf("remote main=%s want %s", remoteMain, res.ToSHA)
	}
	// Both sides' files landed.
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(worktree, name)); err != nil {
			t.Fatalf("missing %s after catch-up: %v", name, err)
		}
	}
}

func TestDirectShipFFCatchUpConflictAbortsClean(t *testing.T) {
	ctx := t.Context()
	remote, main, worktree, branch := setupDirectShipFixture(t)

	if err := os.WriteFile(filepath.Join(worktree, "same.txt"), []byte("session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", "same.txt")
	runGitTest(t, worktree, "commit", "-m", "session")

	advanceRemoteMain(t, remote, "same.txt", "primary\n", "primary")

	before, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before = strings.TrimSpace(before)

	_, err = DirectShipFFCatchUp(ctx, main, worktree, branch, "main")
	if err == nil {
		t.Fatal("want catch-up merge conflict")
	}
	if !strings.Contains(err.Error(), "catch-up merge") {
		t.Fatalf("err: %v", err)
	}

	after, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != before {
		t.Fatalf("HEAD moved after aborted merge: %s → %s", before, after)
	}
	dirty, err := hasTrackedDirt(ctx, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("worktree left dirty after conflict abort")
	}
	// Remote main must still be the primary-side commit, not the session.
	sessionHead := before
	remoteMain, err := gitOutput(ctx, remote, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(remoteMain) == sessionHead {
		t.Fatal("conflict must not push session HEAD")
	}
}

func advanceRemoteMain(t *testing.T, remote, file, contents, msg string) {
	t.Helper()
	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", remote, cloneDir)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runGitTest(t, cloneDir, "checkout", "-B", "main", "origin/main")
	if err := os.WriteFile(filepath.Join(cloneDir, file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, cloneDir, "add", file)
	runGitTest(t, cloneDir, "commit", "-m", msg)
	runGitTest(t, cloneDir, "push", "origin", "HEAD:main")
}

// setupDirectShipFixture returns bare remote, main checkout, managed worktree path, branch.
func setupDirectShipFixture(t *testing.T) (remote, main, worktree, branch string) {
	t.Helper()
	ctx := context.Background()
	// Bare remote.
	remote = filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, t.TempDir(), "init", "--bare", remote)

	// Seed remote via a temp clone.
	seed := t.TempDir()
	runGitTest(t, seed, "init")
	runGitTest(t, seed, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "README")
	runGitTest(t, seed, "commit", "-m", "init")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "-u", "origin", "main")

	// Main checkout (simulates project path).
	main = filepath.Join(t.TempDir(), "main")
	cmd := exec.Command("git", "clone", remote, main)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone main: %v\n%s", err, out)
	}
	runGitTest(t, main, "checkout", "main")

	// Managed worktree on grokwork/tid (default managed prefix).
	data := t.TempDir()
	tr, err := Ensure(ctx, main, data, "proj", "tid-ship")
	if err != nil {
		t.Fatal(err)
	}
	return remote, main, tr.Path, tr.Branch
}
