package gitworktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCherryPickFFKeepsOriginalSHA(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")

	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "ff")
	res, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{b},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA != b {
		t.Fatalf("want ff to original sha, got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != b {
		t.Fatalf("staging=%s want %s", got, b)
	}
	assertGone(t, checkout)
	assertMainCleanOnMain(t, main)
}

func TestCherryPickDoesNotBringIntermediates(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")
	c := commitOnMain(t, main, "c.txt", "c\n", "add c")

	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "skip-mid")
	res, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{c},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA == c {
		t.Fatalf("picking a hop-ahead commit must create a new sha, got %+v", res)
	}
	if runGit(ctx, main, "merge-base", "--is-ancestor", b, "origin/staging") == nil {
		t.Fatal("intermediate b must not be on staging")
	}
	if runGit(ctx, main, "merge-base", "--is-ancestor", c, "origin/staging") == nil {
		t.Fatal("original c must not be on staging (would mean sha-push of tip)")
	}
	assertGone(t, checkout)
}

func TestCherryPickOrdersOldestFirst(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")
	c := commitOnMain(t, main, "c.txt", "c\n", "add c")
	d := commitOnMain(t, main, "d.txt", "d\n", "add d")

	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "order")
	res, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging",
		SHAs: []string{d, c, b}, // newest first
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA != d {
		t.Fatalf("contiguous oldest-first should ff to d, got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != d {
		t.Fatalf("staging=%s want %s", got, d)
	}
}

func TestCherryPickSkipAlreadyOnTarget(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	initSHA := originSHA(t, main, "staging")

	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "skip")
	res, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{initSHA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Noop || res.ToSHA != initSHA {
		t.Fatalf("want noop, got %+v", res)
	}
	if len(res.Skipped) == 0 {
		t.Fatal("expected skipped")
	}
	if got := originSHA(t, main, "staging"); got != initSHA {
		t.Fatalf("remote moved: %s", got)
	}
	assertGone(t, checkout)
}

func TestCherryPickEmptyPatchSkipsAndContinues(t *testing.T) {
	ctx := t.Context()
	remote, main := setupCherryPickFixture(t)
	// Independent same-tree change on staging, then a later unique commit on main.
	side := filepath.Join(t.TempDir(), "side")
	runGitTest(t, t.TempDir(), "clone", remote, side)
	runGitTest(t, side, "config", "user.name", "test")
	runGitTest(t, side, "config", "user.email", "test@example.com")
	runGitTest(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "same.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, side, "add", "same.txt")
	runGitTest(t, side, "commit", "-m", "staging same")
	runGitTest(t, side, "push", "origin", "staging")
	runGitTest(t, main, "fetch", "origin")

	same := commitOnMain(t, main, "same.txt", "same\n", "main same")
	later := commitOnMain(t, main, "later.txt", "later\n", "later")

	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "empty")
	res, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging",
		SHAs: []string{same, later},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop {
		t.Fatalf("later should have applied: %+v", res)
	}
	if runGit(ctx, main, "merge-base", "--is-ancestor", later, "origin/staging") == nil {
		t.Fatal("original later should not be on staging unless it ff'd from a parent that is not staging")
	}
	// later.txt must exist on staging tip
	runGitTest(t, main, "fetch", "origin")
	blob, err := gitOutput(ctx, main, "show", "origin/staging:later.txt")
	if err != nil || strings.TrimSpace(blob) != "later" {
		t.Fatalf("later.txt on staging: %q err=%v", blob, err)
	}
	assertGone(t, checkout)
}

func TestCherryPickConflictParks(t *testing.T) {
	ctx := t.Context()
	remote, main := setupCherryPickFixture(t)

	side := filepath.Join(t.TempDir(), "side")
	runGitTest(t, t.TempDir(), "clone", remote, side)
	runGitTest(t, side, "config", "user.name", "test")
	runGitTest(t, side, "config", "user.email", "test@example.com")
	runGitTest(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "README"), []byte("staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, side, "add", "README")
	runGitTest(t, side, "commit", "-m", "staging readme")
	runGitTest(t, side, "push", "origin", "staging")
	runGitTest(t, main, "fetch", "origin")
	before := originSHA(t, main, "staging")

	conflictSHA := commitOnMain(t, main, "README", "mainline\n", "main readme")
	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "conflict")
	_, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{conflictSHA},
	})
	if err == nil {
		t.Fatal("expected conflict")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(err.Error(), checkout) || strings.Contains(err.Error(), main) {
		t.Fatalf("error leaked path: %v", err)
	}
	if !strings.Contains(err.Error(), "README") {
		t.Fatalf("want conflicted file in error: %v", err)
	}
	if got := originSHA(t, main, "staging"); got != before {
		t.Fatalf("remote moved on conflict: %s → %s", before, got)
	}
	cerr, ok := errors.AsType[*ConflictError](err)
	if !ok {
		t.Fatalf("want ConflictError, got %T %v", err, err)
	}
	if !slices.Contains(cerr.Files, "README") {
		t.Fatalf("files=%v", cerr.Files)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("checkout should remain: %v", err)
	}
	if !SequencerLive(ctx, checkout) {
		t.Fatal("expected CHERRY_PICK_HEAD in checkout")
	}
	assertMainCleanOnMain(t, main)
	if _, err := os.Stat(filepath.Join(main, ".git", "CHERRY_PICK_HEAD")); err == nil {
		t.Fatal("sequencer leaked into main checkout")
	}
}

func TestCherryPickAbortDeletes(t *testing.T) {
	ctx := t.Context()
	_, main, checkout, before, _ := parkConflict(t)
	if err := AbortCherryPick(ctx, main, checkout); err != nil {
		t.Fatal(err)
	}
	assertGone(t, checkout)
	if got := originSHA(t, main, "staging"); got != before {
		t.Fatalf("abort moved remote: %s", got)
	}
	assertMainCleanOnMain(t, main)
}

func TestCherryPickContinuePushes(t *testing.T) {
	ctx := t.Context()
	_, main, checkout, _, conflictSHA := parkConflict(t)
	if err := WriteWorkingFile(checkout, "README", []byte("mainline\n")); err != nil {
		t.Fatal(err)
	}
	res, err := ContinueCherryPick(ctx, ContinueOpts{
		Repo: main, Checkout: checkout, Target: "staging",
		FromSHA: originSHA(t, main, "staging"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA == "" {
		t.Fatalf("res=%+v", res)
	}
	got := originSHA(t, main, "staging")
	if got == conflictSHA {
		// new SHA from -x is expected; content must match
	}
	blob, err := gitOutput(ctx, main, "show", "origin/staging:README")
	if err != nil || strings.TrimSpace(blob) != "mainline" {
		t.Fatalf("README on staging: %q err=%v", blob, err)
	}
	assertGone(t, checkout)
}

func TestCherryPickContinueStaleTarget(t *testing.T) {
	ctx := t.Context()
	remote, main, checkout, from, _ := parkConflict(t)
	side := filepath.Join(t.TempDir(), "mover")
	runGitTest(t, t.TempDir(), "clone", remote, side)
	runGitTest(t, side, "config", "user.name", "test")
	runGitTest(t, side, "config", "user.email", "test@example.com")
	runGitTest(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "extra.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, side, "add", "extra.txt")
	runGitTest(t, side, "commit", "-m", "move staging")
	runGitTest(t, side, "push", "origin", "staging")
	moved := originSHA(t, main, "staging")
	if moved == from {
		t.Fatal("expected staging to move")
	}
	if err := WriteWorkingFile(checkout, "README", []byte("mainline\n")); err != nil {
		t.Fatal(err)
	}
	_, err := ContinueCherryPick(ctx, ContinueOpts{
		Repo: main, Checkout: checkout, Target: "staging", FromSHA: from,
	})
	if !errors.Is(err, ErrTargetMoved) {
		t.Fatalf("want ErrTargetMoved, got %v", err)
	}
	if got := originSHA(t, main, "staging"); got != moved {
		t.Fatalf("continue must not push over moved target: %s", got)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("stale continue must leave checkout: %v", err)
	}
}

func TestCherryPickContinueOursIsNoopAndCleans(t *testing.T) {
	ctx := t.Context()
	_, main, checkout, from, _ := parkConflict(t)
	if err := CheckoutConflictSide(ctx, checkout, "README", "ours"); err != nil {
		t.Fatal(err)
	}
	res, err := ContinueCherryPick(ctx, ContinueOpts{
		Repo: main, Checkout: checkout, Target: "staging", FromSHA: from, Current: originSHA(t, main, "staging"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Noop {
		t.Fatalf("taking ours on the only commit should be a no-op, got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != from {
		t.Fatalf("ours-continue pushed: %s", got)
	}
	assertGone(t, checkout)
}

func TestCherryPickContinueLeftoverMarkers(t *testing.T) {
	ctx := t.Context()
	_, main, checkout, from, _ := parkConflict(t)
	if err := WriteWorkingFile(checkout, "README", []byte("<<<<<<< ours\nstaging\n=======\nmainline\n>>>>>>> theirs\n")); err != nil {
		t.Fatal(err)
	}
	_, err := ContinueCherryPick(ctx, ContinueOpts{
		Repo: main, Checkout: checkout, Target: "staging", FromSHA: from,
	})
	if !errors.Is(err, ErrUnresolvedConflicts) {
		t.Fatalf("want ErrUnresolvedConflicts, got %v", err)
	}
	if !SequencerLive(ctx, checkout) {
		t.Fatal("leftover markers must not abort the sequencer")
	}
	if got := originSHA(t, main, "staging"); got != from {
		t.Fatalf("remote moved: %s", got)
	}
}

func TestCheckoutConflictSideOursTheirs(t *testing.T) {
	ctx := t.Context()
	_, main, checkout, from, _ := parkConflict(t)
	if err := CheckoutConflictSide(ctx, checkout, "README", "ours"); err != nil {
		t.Fatal(err)
	}
	ours, err := ReadWorkingFile(checkout, "README", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(ours)) != "staging" {
		t.Fatalf("ours=%q", ours)
	}
	if err := CheckoutConflictSide(ctx, checkout, "README", "theirs"); err != nil {
		t.Fatal(err)
	}
	theirs, err := ReadWorkingFile(checkout, "README", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(theirs)) != "mainline" {
		t.Fatalf("theirs=%q", theirs)
	}
	if !SequencerLive(ctx, checkout) {
		t.Fatal("ours/theirs must not stage or continue")
	}
	if got := originSHA(t, main, "staging"); got != from {
		t.Fatalf("remote moved: %s", got)
	}
}

func TestContainedRelPathRejectsEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := ContainedRelPath(dir, "../secret"); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := ContainedRelPath(dir, "/etc/passwd"); err == nil {
		t.Fatal("expected reject abs")
	}
	if _, err := ContainedRelPath(dir, "foo/../../secret"); err == nil {
		t.Fatal("expected reject dotted escape")
	}
	got, err := ContainedRelPath(dir, "README")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "README") {
		t.Fatalf("got %s", got)
	}
}

func TestCherryPickContinueHasNoForce(t *testing.T) {
	src, err := os.ReadFile("cherrypick_continue.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "--force") {
		t.Fatal("cherrypick_continue.go must not mention --force")
	}
}

func parkConflict(t *testing.T) (remote, main, checkout, fromSHA, conflictSHA string) {
	t.Helper()
	ctx := t.Context()
	remote, main = setupCherryPickFixture(t)
	side := filepath.Join(t.TempDir(), "side")
	runGitTest(t, t.TempDir(), "clone", remote, side)
	runGitTest(t, side, "config", "user.name", "test")
	runGitTest(t, side, "config", "user.email", "test@example.com")
	runGitTest(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "README"), []byte("staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, side, "add", "README")
	runGitTest(t, side, "commit", "-m", "staging readme")
	runGitTest(t, side, "push", "origin", "staging")
	runGitTest(t, main, "fetch", "origin")
	fromSHA = originSHA(t, main, "staging")
	conflictSHA = commitOnMain(t, main, "README", "mainline\n", "main readme")
	checkout = CherryPickCheckoutPath(t.TempDir(), "proj", "conflict")
	_, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{conflictSHA},
	})
	if _, ok := errors.AsType[*ConflictError](err); !ok {
		t.Fatalf("park: %v", err)
	}
	return remote, main, checkout, fromSHA, conflictSHA
}

func TestCherryPickRefusesMissingTarget(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	sha := commitOnMain(t, main, "x.txt", "x\n", "x")
	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "missing")
	_, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "no-such-target", SHAs: []string{sha},
	})
	if err == nil {
		t.Fatal("expected refuse")
	}
	if !strings.Contains(err.Error(), "no-such-target") {
		t.Fatalf("err: %v", err)
	}
	out, _ := gitOutput(ctx, main, "ls-remote", "--heads", "origin", "no-such-target")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("created remote branch: %q", out)
	}
}

func TestCherryPickRefusesPrimary(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	sha := commitOnMain(t, main, "x.txt", "x\n", "x")
	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "primary")
	_, err := CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "main", SHAs: []string{sha}, PreferredPrimary: "main",
	})
	if err == nil {
		t.Fatal("expected refuse primary")
	}
	if !strings.Contains(err.Error(), "primary") {
		t.Fatalf("err: %v", err)
	}
}

func TestCherryPickRefusesMergeCommit(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	runGitTest(t, main, "checkout", "-b", "side")
	writeCommit(t, main, "side.txt", "s\n", "side")
	runGitTest(t, main, "checkout", "main")
	if err := runGit(ctx, main, "merge", "--no-ff", "-m", "merge side", "side"); err != nil {
		t.Fatal(err)
	}
	merge, err := gitOutput(ctx, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, main, "push", "origin", "main")
	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "merge")
	_, err = CherryPick(ctx, CherryPickOpts{
		Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{strings.TrimSpace(merge)},
	})
	if err == nil {
		t.Fatal("expected refuse merge")
	}
	if !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("err: %v", err)
	}
}

func TestCherryPickRefusesNonHexSHA(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	checkout := CherryPickCheckoutPath(t.TempDir(), "proj", "hex")
	for _, sha := range []string{"HEAD", "staging", "origin/main", "abc"} {
		_, err := CherryPick(ctx, CherryPickOpts{
			Repo: main, Checkout: checkout, Target: "staging", SHAs: []string{sha},
		})
		if err == nil {
			t.Fatalf("accepted %q", sha)
		}
	}
}

func TestCherryPickPushHasNoForce(t *testing.T) {
	src, err := os.ReadFile("cherrypick.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "--force") {
		t.Fatal("cherrypick.go must not mention --force")
	}
}

func TestIsHexSHA(t *testing.T) {
	t.Parallel()
	ok := []string{"abcd123", "ABCDEF0", strings.Repeat("a", 40)}
	for _, s := range ok {
		if !IsHexSHA(s) {
			t.Errorf("IsHexSHA(%q)=false", s)
		}
	}
	bad := []string{"", "HEAD", "staging", "abc", "abcd123g", strings.Repeat("a", 41)}
	for _, s := range bad {
		if IsHexSHA(s) {
			t.Errorf("IsHexSHA(%q)=true", s)
		}
	}
}

func setupCherryPickFixture(t *testing.T) (remote, main string) {
	t.Helper()
	remote = filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, t.TempDir(), "init", "--bare", remote)

	seed := t.TempDir()
	runGitTest(t, seed, "init")
	runGitTest(t, seed, "branch", "-M", "main")
	runGitTest(t, seed, "config", "user.name", "test")
	runGitTest(t, seed, "config", "user.email", "test@example.com")
	runGitTest(t, seed, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "README")
	runGitTest(t, seed, "commit", "-m", "init")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "-u", "origin", "main")
	runGitTest(t, seed, "branch", "staging")
	runGitTest(t, seed, "push", "-u", "origin", "staging")

	main = filepath.Join(t.TempDir(), "main")
	cmd := exec.Command("git", "clone", remote, main)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone main: %v\n%s", err, out)
	}
	runGitTest(t, main, "checkout", "main")
	runGitTest(t, main, "config", "user.name", "test")
	runGitTest(t, main, "config", "user.email", "test@example.com")
	runGitTest(t, main, "config", "commit.gpgsign", "false")
	return remote, main
}

func commitOnMain(t *testing.T, main, file, contents, msg string) string {
	t.Helper()
	runGitTest(t, main, "checkout", "main")
	sha := writeCommit(t, main, file, contents, msg)
	runGitTest(t, main, "push", "origin", "main")
	return sha
}

func writeCommit(t *testing.T, repo, file, contents, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", file)
	cmd := exec.Command("git", "-C", repo, "commit", "-m", msg)
	when := time.Now().UTC().Add(time.Duration(len(msg)+len(file)) * time.Second).Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	sha, err := gitOutput(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(sha)
}

func originSHA(t *testing.T, repo, branch string) string {
	t.Helper()
	runGitTest(t, repo, "fetch", "origin")
	sha, err := gitOutput(t.Context(), repo, "rev-parse", "--verify", "origin/"+branch+"^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(sha)
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path should be gone: %s (%v)", path, err)
	}
}

func assertMainCleanOnMain(t *testing.T, main string) {
	t.Helper()
	ctx := t.Context()
	br, err := gitOutput(ctx, main, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(br) != "main" {
		t.Fatalf("main checkout on %q", br)
	}
	dirty, err := hasTrackedDirt(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("main checkout is dirty")
	}
}
