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
		{"grok/web/w_abc123", true},
		{"grok/web/unit-1", true},
		{"main", false},
		{"master", false},
		{"develop", false},
		{"feature/foo", false},
		{"grok/discord/", false},
		{"grok/discord", false},
		{"grok/discord/..", false},
		{"grok/discord/../main", false},
		{"grok/web/", false},
		{"grok/web", false},
		{"grok/web/..", false},
		{"grok/other/x", false},
		{"", false},
		{"  ", false},
	}
	for _, tt := range tests {
		if got := IsManagedBranch(tt.branch); got != tt.ok {
			t.Errorf("IsManagedBranch(%q)=%v want %v", tt.branch, got, tt.ok)
		}
	}
}

func TestEnsureWebPrefix(t *testing.T) {
	repo := initTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()
	unitID := "w_testhub01"

	tr, err := EnsureWith(ctx, repo, data, "app", unitID, EnsureOpts{BranchPrefix: WebBranchPrefix})
	if err != nil {
		t.Fatal(err)
	}
	wantBranch := "grok/web/" + unitID
	if tr.Branch != wantBranch {
		t.Fatalf("branch=%q want %q", tr.Branch, wantBranch)
	}
	if !IsRepo(tr.Path) {
		t.Fatal("worktree not a repo")
	}
	if !IsManagedBranch(tr.Branch) {
		t.Fatal("web branch should be managed")
	}

	// EnsureWith empty prefix still picks web from unit id form.
	tr2, err := EnsureWith(ctx, repo, data, "app", unitID, EnsureOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Branch != wantBranch || tr2.Path != tr.Path {
		t.Fatalf("reuse via unit id: %+v vs %+v", tr2, tr)
	}

	if err := Remove(ctx, repo, tr.Path, tr.Branch); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+wantBranch)
	if cmd.Run() == nil {
		t.Fatal("web branch still exists after remove")
	}
}

func TestEnsureDiscordStillDefault(t *testing.T) {
	repo := initTestRepo(t)
	tr, err := Ensure(context.Background(), repo, t.TempDir(), "app", "1524726013211316294")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Branch != "grok/discord/1524726013211316294" {
		t.Fatalf("branch=%q", tr.Branch)
	}
}

func TestNewWebUnitIDAndHelpers(t *testing.T) {
	id := NewWebUnitID()
	if !IsWebUnitID(id) {
		t.Fatalf("NewWebUnitID=%q not web form", id)
	}
	if PrefixForUnitID(id) != WebBranchPrefix {
		t.Fatal("prefix for web unit")
	}
	if BranchNameForUnit(id) != WebBranchPrefix+id {
		t.Fatalf("branch=%q", BranchNameForUnit(id))
	}
	if BranchName("123") != DiscordBranchPrefix+"123" {
		t.Fatal("discord BranchName regression")
	}
	if IsWebUnitID("1234567890") || IsWebUnitID("w_") || IsWebUnitID("") {
		t.Fatal("false positive web unit id")
	}
	if PrefixFromBranch("grok/web/w_x") != WebBranchPrefix {
		t.Fatal("PrefixFromBranch web")
	}
	if PrefixFromBranch("grok/discord/1") != DiscordBranchPrefix {
		t.Fatal("PrefixFromBranch discord")
	}
	if PrefixFromBranch("main") != "" {
		t.Fatal("unmanaged")
	}
}

func TestRemoveWebManagedBranch(t *testing.T) {
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
	run("branch", "grok/web/w_delme")
	if err := Remove(ctx, repo, "", "grok/web/w_delme"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/grok/web/w_delme")
	if cmd.Run() == nil {
		t.Fatal("web managed branch should have been deleted")
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
