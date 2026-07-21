package gitworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveNewBranchStartPrefersOrigin(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	if got := resolveNewBranchStart(ctx, repo); got != "HEAD" {
		t.Fatalf("no origin: got %q want HEAD", got)
	}

	// Simulate origin/main without a real remote.
	runGitTest(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	if got := resolveNewBranchStart(ctx, repo); got != "origin/main" {
		t.Fatalf("got %q want origin/main", got)
	}

	runGitTest(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if got := resolveNewBranchStart(ctx, repo); got != "origin/main" {
		t.Fatalf("symbolic-ref: got %q", got)
	}
}

func TestMaybeFetchIntervalThrottle(t *testing.T) {
	resetFetchStateForTest()
	t.Cleanup(resetFetchStateForTest)

	remote, seed := initRemotePair(t)
	repo := t.TempDir()
	runGitTest(t, repo, "clone", remote, ".")
	resetFetchStateForTest()

	ctx := context.Background()
	if err := MaybeFetch(ctx, repo, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Second call within interval must not error; throttle skips git fetch.
	if err := MaybeFetch(ctx, repo, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := MaybeFetch(ctx, repo, 0); err != nil {
		t.Fatal(err)
	}
	// NoteFetched from outside should also throttle.
	resetFetchStateForTest()
	NoteFetched(repo)
	runGitTest(t, repo, "remote", "set-url", "origin", "/nonexistent-remote-path")
	if err := MaybeFetch(ctx, repo, time.Hour); err != nil {
		t.Fatalf("should skip after NoteFetched: %v", err)
	}
	_ = seed
}

func TestEnsureFetchesAndStartsFromOrigin(t *testing.T) {
	resetFetchStateForTest()
	t.Cleanup(resetFetchStateForTest)

	remote, seed := initRemotePair(t)

	// Freeze a local clone at the first tip.
	repo := t.TempDir()
	runGitTest(t, repo, "clone", remote, ".")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	localSHA := strings.TrimSpace(string(out))

	// Advance remote without local fetch.
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "a.txt")
	runGitTest(t, seed, "commit", "-m", "v2")
	runGitTest(t, seed, "push", "origin", "main")
	out, err = exec.Command("git", "-C", seed, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := strings.TrimSpace(string(out))
	if wantSHA == localSHA {
		t.Fatal("setup failed: remote should have advanced")
	}

	data := t.TempDir()
	ctx := context.Background()
	tr, err := EnsureWith(ctx, repo, data, "app", "fetch-tid", EnsureOpts{
		FetchInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command("git", "-C", tr.Path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != wantSHA {
		t.Fatalf("worktree HEAD=%s want remote tip %s (local was %s)",
			strings.TrimSpace(string(got)), wantSHA, localSHA)
	}

	// Reuse must not require fetch (and keeps same path).
	tr2, err := EnsureWith(ctx, repo, data, "app", "fetch-tid", EnsureOpts{
		FetchInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Path != tr.Path {
		t.Fatalf("reuse path %q vs %q", tr2.Path, tr.Path)
	}
}

// initRemotePair returns a bare remote and a seed checkout with one commit on main.
func initRemotePair(t *testing.T) (remote, seed string) {
	t.Helper()
	remote = t.TempDir()
	runGitTest(t, remote, "init", "--bare")
	seed = t.TempDir()
	runGitTest(t, seed, "init")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "a.txt")
	runGitTest(t, seed, "commit", "-m", "v1")
	runGitTest(t, seed, "branch", "-M", "main")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "-u", "origin", "main")
	// Bare default branch is often master; point HEAD at main so clone checks out.
	runGitTest(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote, seed
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
