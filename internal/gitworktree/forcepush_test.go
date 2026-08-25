package gitworktree

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestForcePushFF(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")
	from := originSHA(t, main, "staging")

	res, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: b, PreferredPrimary: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.Forced || res.ToSHA != b || res.FromSHA != from {
		t.Fatalf("got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != b {
		t.Fatalf("staging=%s want %s", got, b)
	}
	assertMainCleanOnMain(t, main)
}

func TestForcePushNoop(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	cur := originSHA(t, main, "staging")
	res, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: cur, PreferredPrimary: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Noop || res.Forced || res.ToSHA != cur {
		t.Fatalf("got %+v", res)
	}
	assertMainCleanOnMain(t, main)
}

func TestForcePushRewind(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	old := originSHA(t, main, "staging")
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")
	if _, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: b, PreferredPrimary: "main"}); err != nil {
		t.Fatal(err)
	}
	res, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: old, PreferredPrimary: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || !res.Forced || res.ToSHA != old || res.FromSHA != b {
		t.Fatalf("got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != old {
		t.Fatalf("staging=%s want %s", got, old)
	}
	assertMainCleanOnMain(t, main)
}

func TestForcePushSideways(t *testing.T) {
	ctx := t.Context()
	remote, main := setupCherryPickFixture(t)
	side := filepath.Join(t.TempDir(), "side")
	runGitTest(t, t.TempDir(), "clone", remote, side)
	runGitTest(t, side, "config", "user.name", "test")
	runGitTest(t, side, "config", "user.email", "test@example.com")
	runGitTest(t, side, "checkout", "staging")
	writeCommit(t, side, "s.txt", "side\n", "staging unique")
	runGitTest(t, side, "push", "origin", "staging")
	runGitTest(t, main, "fetch", "origin")
	b := commitOnMain(t, main, "b.txt", "b\n", "main unique")

	res, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: b, PreferredPrimary: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || !res.Forced || res.ToSHA != b {
		t.Fatalf("got %+v", res)
	}
	if got := originSHA(t, main, "staging"); got != b {
		t.Fatalf("staging=%s want %s", got, b)
	}
	assertMainCleanOnMain(t, main)
}

func TestForcePushStaleLease(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	b := commitOnMain(t, main, "b.txt", "b\n", "add b")
	cur := originSHA(t, main, "staging")
	err := pushTarget(ctx, main, b, "staging", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false)
	if err == nil {
		t.Fatal("expected lease refusal")
	}
	if got := originSHA(t, main, "staging"); got != cur {
		t.Fatalf("remote moved: %s", got)
	}
}

func TestForcePushRefusesMissingTarget(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	sha := commitOnMain(t, main, "x.txt", "x\n", "x")
	_, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "no-such-target", SHA: sha, PreferredPrimary: "main"})
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

func TestForcePushRefusesPrimary(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	sha := commitOnMain(t, main, "x.txt", "x\n", "x")
	_, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "main", SHA: sha, PreferredPrimary: "main"})
	if err == nil {
		t.Fatal("expected refuse primary")
	}
	if !strings.Contains(err.Error(), "primary") {
		t.Fatalf("err: %v", err)
	}
}

func TestForcePushRefusesManaged(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	sha := commitOnMain(t, main, "x.txt", "x\n", "x")
	_, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "grokwork/abc", SHA: sha})
	if err == nil {
		t.Fatal("expected refuse")
	}
}

func TestForcePushRefusesNonCommit(t *testing.T) {
	ctx := t.Context()
	_, main := setupCherryPickFixture(t)
	for _, sha := range []string{"HEAD", "staging", "origin/main", "abc", ""} {
		_, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: sha})
		if err == nil {
			t.Fatalf("accepted %q", sha)
		}
	}
	runGitTest(t, main, "tag", "-a", "v1", "-m", "annotated")
	tag, err := gitOutput(ctx, main, "rev-parse", "v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: tag, PreferredPrimary: "main"})
	if err == nil {
		t.Fatal("expected refuse annotated tag object")
	}
	if !strings.Contains(err.Error(), "tag") {
		t.Fatalf("err: %v", err)
	}
}

func TestForcePushAllowsMergeCommit(t *testing.T) {
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
	res, err := ForcePush(ctx, ForcePushOpts{Repo: main, Target: "staging", SHA: merge, PreferredPrimary: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Noop || res.ToSHA != merge {
		t.Fatalf("got %+v", res)
	}
}

func TestForcePushArgs(t *testing.T) {
	t.Parallel()
	ff := forcePushArgs("abc", "staging", "def", true)
	joined := strings.Join(ff, " ")
	if strings.Contains(joined, "--force") || strings.Contains(joined, "+") {
		t.Fatalf("ff argv: %v", ff)
	}
	if got, want := ff, []string{"push", "origin", "abc:refs/heads/staging"}; !slices.Equal(got, want) {
		t.Fatalf("ff=%v want %v", got, want)
	}
	lease := forcePushArgs("abc", "staging", "def", false)
	joined = strings.Join(lease, " ")
	if strings.Contains(joined, "+") {
		t.Fatalf("lease argv +: %v", lease)
	}
	if got, want := lease, []string{
		"push",
		"--force-with-lease=refs/heads/staging:def",
		"origin",
		"abc:refs/heads/staging",
	}; !slices.Equal(got, want) {
		t.Fatalf("lease=%v want %v", got, want)
	}
}

func TestForcePushSourceHasLeaseNotBareForce(t *testing.T) {
	src, err := os.ReadFile("forcepush.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "--force-with-lease=") {
		t.Fatal("forcepush.go must use explicit --force-with-lease=")
	}
	stripped := strings.ReplaceAll(body, "--force-with-lease=", "")
	if strings.Contains(stripped, "--force") {
		t.Fatal("forcepush.go must not mention bare --force")
	}
}
