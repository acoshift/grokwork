package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanAdoptBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		branch, primary string
		ok              bool
	}{
		{"feat-x", "", true},
		{"feat/foo", "", true},
		{"refs/heads/feat-x", "", true},
		{"", "", false},
		{"main", "", false},
		{"master", "", false},
		{"HEAD", "", false},
		{"prod", "prod", false},
		{"feat-x", "prod", true},
		{"grokwork/abc", "", false},
		{"grok/web/w_1", "", false},
		{"-bad", "", false},
		{"foo..bar", "", false},
		{"foo bar", "", false},
	}
	for _, tc := range cases {
		got := CanAdoptBranch(tc.branch, tc.primary)
		if got != tc.ok {
			t.Errorf("CanAdoptBranch(%q, %q)=%v want %v", tc.branch, tc.primary, got, tc.ok)
		}
	}
}

func TestEnsureAdoptedUsesExistingBranch(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	primary := currentBranch(t, repo)
	runGitTest(t, repo, "checkout", "-b", "feat-x")
	if err := os.WriteFile(filepath.Join(repo, "pr.txt"), []byte("on-pr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "pr.txt")
	runGitTest(t, repo, "commit", "-m", "pr work")
	runGitTest(t, repo, "checkout", primary)

	data := t.TempDir()
	tr, err := EnsureAdopted(ctx, repo, data, "app", "w_import1", AdoptOpts{Branch: "feat-x", PullNumber: 12})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Branch != "feat-x" {
		t.Fatalf("branch=%q", tr.Branch)
	}
	if headBranch(ctx, tr.Path) != "feat-x" {
		t.Fatalf("HEAD=%q", headBranch(ctx, tr.Path))
	}
	body, err := os.ReadFile(filepath.Join(tr.Path, "pr.txt"))
	if err != nil || string(body) != "on-pr\n" {
		t.Fatalf("pr.txt=%q err=%v", body, err)
	}
	if branchExists(ctx, repo, WebBranchPrefix+"w_import1") || branchExists(ctx, repo, BranchPrefix+"w_import1") {
		t.Fatal("must not mint a managed branch")
	}

	tr2, err := EnsureAdopted(ctx, repo, data, "app", "w_import1", AdoptOpts{Branch: "feat-x"})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Path != tr.Path || tr2.Branch != "feat-x" {
		t.Fatalf("reuse %+v want %+v", tr2, tr)
	}

	if err := Remove(ctx, repo, tr.Path, "feat-x"); err != nil {
		t.Fatalf("Remove worktree: %v", err)
	}
	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	if !branchExists(ctx, repo, "feat-x") {
		t.Fatal("adopted branch must survive worktree Remove")
	}
}

func TestEnsureAdoptedRefusesPrimaryAndManaged(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	data := t.TempDir()
	primary := currentBranch(t, repo)
	if _, err := EnsureAdopted(ctx, repo, data, "app", "w_1", AdoptOpts{Branch: primary}); err == nil {
		t.Fatal("expected refuse primary")
	}
	if _, err := EnsureAdopted(ctx, repo, data, "app", "w_1", AdoptOpts{Branch: "grokwork/x"}); err == nil {
		t.Fatal("expected refuse managed")
	}
}

func TestEnsureAdoptedRefusesCheckedOutElsewhere(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	runGitTest(t, repo, "checkout", "-b", "feat-busy")
	data := t.TempDir()
	_, err := EnsureAdopted(ctx, repo, data, "app", "w_busy", AdoptOpts{Branch: "feat-busy"})
	if err == nil {
		t.Fatal("expected conflict")
	}
	if !strings.Contains(err.Error(), "already checked out") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureAdoptedFromOriginRef(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	primary := currentBranch(t, repo)
	runGitTest(t, repo, "checkout", "-b", "feat-remote")
	runGitTest(t, repo, "commit", "--allow-empty", "-m", "remote tip")
	sha := strings.TrimSpace(gitOutputOrFatal(t, repo, "rev-parse", "HEAD"))
	// Simulate origin/feat-remote without a real remote; drop the local branch.
	runGitTest(t, repo, "update-ref", "refs/remotes/origin/feat-remote", sha)
	runGitTest(t, repo, "checkout", primary)
	runGitTest(t, repo, "branch", "-D", "feat-remote")

	data := t.TempDir()
	tr, err := EnsureAdopted(ctx, repo, data, "app", "w_from_origin", AdoptOpts{Branch: "feat-remote"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Branch != "feat-remote" {
		t.Fatalf("branch=%q", tr.Branch)
	}
	if !branchExists(ctx, repo, "feat-remote") {
		t.Fatal("local tracking branch should have been created")
	}
}

func currentBranch(t *testing.T, repo string) string {
	t.Helper()
	out := gitOutputOrFatal(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out)
}

func gitOutputOrFatal(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
