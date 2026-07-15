package gitworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureReuseAndRemove(t *testing.T) {
	repo := initTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()

	tr, err := Ensure(ctx, repo, data, "app", "111")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Branch != "grok/discord/111" {
		t.Fatalf("branch=%q", tr.Branch)
	}
	if !IsRepo(tr.Path) {
		t.Fatal("worktree not a repo")
	}
	marker := filepath.Join(tr.Path, "only-wt.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "only-wt.txt")); !os.IsNotExist(err) {
		t.Fatal("marker leaked into main worktree path")
	}

	tr2, err := Ensure(ctx, repo, data, "app", "111")
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Path != tr.Path {
		t.Fatalf("reuse path %q vs %q", tr2.Path, tr.Path)
	}

	trB, err := Ensure(ctx, repo, data, "app", "222")
	if err != nil {
		t.Fatal(err)
	}
	if trB.Path == tr.Path {
		t.Fatal("threads should not share worktree path")
	}

	if err := Remove(ctx, repo, tr.Path, tr.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatalf("path still exists: %v", err)
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+tr.Branch)
	if cmd.Run() == nil {
		t.Fatal("branch still exists after remove")
	}
}

func TestEnsureNotARepo(t *testing.T) {
	dir := t.TempDir()
	_, err := Ensure(context.Background(), dir, t.TempDir(), "p", "1")
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
}

func TestSanitizePathSegment(t *testing.T) {
	if got := sanitizePathSegment("my app"); got != "my_app" {
		t.Fatalf("got %q", got)
	}
	got := sanitizePathSegment("../../../x")
	if strings.Contains(got, "/") || strings.Contains(got, string(filepath.Separator)) {
		t.Fatalf("unsafe %q", got)
	}
	if got == "." || got == ".." {
		t.Fatalf("unsafe %q", got)
	}
}

func TestTerminalPRState(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		done   bool
		state  string
	}{
		{"empty", nil, false, ""},
		{"open only", []string{"OPEN"}, false, ""},
		{"merged", []string{"MERGED"}, true, "MERGED"},
		{"closed", []string{"CLOSED"}, true, "CLOSED"},
		{"open wins over merged", []string{"MERGED", "OPEN"}, false, ""},
		{"merged preferred over closed", []string{"CLOSED", "MERGED"}, true, "MERGED"},
		{"case insensitive", []string{"merged"}, true, "MERGED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, state := terminalPRState(tt.states)
			if done != tt.done || state != tt.state {
				t.Fatalf("got done=%v state=%q want done=%v state=%q", done, state, tt.done, tt.state)
			}
		})
	}
}

func TestCleanupIfPRDoneNoTree(t *testing.T) {
	repo := initTestRepo(t)
	cleaned, state, err := CleanupIfPRDone(context.Background(), repo, t.TempDir(), "app", "999")
	if err != nil {
		t.Fatal(err)
	}
	if cleaned || state != "" {
		t.Fatalf("expected no cleanup, got cleaned=%v state=%q", cleaned, state)
	}
}

func TestIsManagedBranch(t *testing.T) {
	tests := []struct {
		branch string
		ok     bool
	}{
		{"grok/discord/123", true},
		{"grok/discord/1524726013211316294", true},
		{"main", false},
		{"master", false},
		{"develop", false},
		{"feature/foo", false},
		{"grok/discord/", false},
		{"grok/discord", false},
		{"grok/discord/..", false},
		{"grok/discord/../main", false},
		{"", false},
		{"  ", false},
	}
	for _, tt := range tests {
		if got := IsManagedBranch(tt.branch); got != tt.ok {
			t.Errorf("IsManagedBranch(%q)=%v want %v", tt.branch, got, tt.ok)
		}
	}
}

func TestRemoveRefusesUnprotectedBranch(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
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
	run("branch", "main-copy")

	err := Remove(ctx, repo, "", "main")
	if err == nil {
		t.Fatal("expected error refusing to delete main")
	}
	if !strings.Contains(err.Error(), "unprotected") {
		t.Fatalf("expected unprotected error, got %v", err)
	}
	err = Remove(ctx, repo, "", "main-copy")
	if err == nil {
		t.Fatal("expected error refusing to delete main-copy")
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/main-copy")
	if cmd.Run() != nil {
		t.Fatal("main-copy was deleted despite protection")
	}

	run("branch", "grok/discord/protect-test")
	if err := Remove(ctx, repo, "", "grok/discord/protect-test"); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/grok/discord/protect-test")
	if cmd.Run() == nil {
		t.Fatal("managed branch should have been deleted")
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
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
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}
