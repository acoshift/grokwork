package gitworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSessionWorktreePathPrefersLiveSessionThenCanonical(t *testing.T) {
	data := t.TempDir()
	// Real worktree under current dataDir; session still has old absolute path.
	repo := initTestRepo(t)
	tr, err := Ensure(context.Background(), repo, data, "app", "tid1")
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(t.TempDir(), "grok-discord", "data", "worktrees", "app", "tid1")

	path, onDisk := ResolveSessionWorktreePath(data, "app", "tid1", stale, repo)
	if !onDisk || path != tr.Path {
		t.Fatalf("want canonical live path, got path=%q onDisk=%v want %q", path, onDisk, tr.Path)
	}

	// Prefer still-valid session cwd over canonical.
	tr2, err := Ensure(context.Background(), repo, data, "app", "tid2")
	if err != nil {
		t.Fatal(err)
	}
	// Session points at a custom live path (same as worktree root).
	path, onDisk = ResolveSessionWorktreePath(data, "app", "tid2", tr2.Path, repo)
	if !onDisk || path != tr2.Path {
		t.Fatalf("want session cwd, got path=%q onDisk=%v", path, onDisk)
	}

	// Empty dir under dataDir is NOT onDisk (would otherwise git-climb to a parent repo).
	empty := WorktreePath(data, "app", "empty-shell")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	path, onDisk = ResolveSessionWorktreePath(data, "app", "empty-shell", "", repo)
	if onDisk || path != empty {
		t.Fatalf("want empty shell not onDisk, got path=%q onDisk=%v", path, onDisk)
	}

	// Neither exists → canonical, not on disk.
	path, onDisk = ResolveSessionWorktreePath(data, "app", "missing", "/old/gone", repo)
	if onDisk || path != WorktreePath(data, "app", "missing") {
		t.Fatalf("want missing canonical, got path=%q onDisk=%v", path, onDisk)
	}
}

func TestIsRepoRequiresWorktreeRoot(t *testing.T) {
	repo := initTestRepo(t)
	if !IsRepo(repo) {
		t.Fatal("main repo should be a worktree root")
	}
	// Nested non-git dir must not inherit parent (bot dataDir is under grokwork).
	nested := filepath.Join(repo, "data", "worktrees", "proj", "tid")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsRepo(nested) {
		t.Fatal("nested empty dir must not count as repo root")
	}
	if IsRepo("") {
		t.Fatal("empty path")
	}
}

func TestResolveLocalRepoSingleAndMulti(t *testing.T) {
	ctx := context.Background()
	// Single-repo: project path is the git root.
	single := initTestRepo(t)
	got, err := ResolveLocalRepo(ctx, single, "acme", "app")
	if err != nil || got != single {
		t.Fatalf("single: got %q err=%v want %q", got, err, single)
	}

	// Multi-repo folder: parent has no .git; children are named after repos.
	root := t.TempDir()
	api := filepath.Join(root, "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal git root without full initTestRepo (just .git dir).
	if err := os.Mkdir(filepath.Join(api, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(root, "console")
	if err := os.MkdirAll(console, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(console, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err = ResolveLocalRepo(ctx, root, "deploys-app", "api")
	if err != nil || got != api {
		t.Fatalf("multi api: got %q err=%v want %q", got, err, api)
	}
	got, err = ResolveLocalRepo(ctx, root, "deploys-app", "console")
	if err != nil || got != console {
		t.Fatalf("multi console: got %q err=%v want %q", got, err, console)
	}
	if _, err := ResolveLocalRepo(ctx, root, "deploys-app", "missing"); err == nil {
		t.Fatal("expected error for missing child")
	}
	if _, err := ResolveLocalRepo(ctx, root, "", ""); err == nil {
		t.Fatal("expected error for empty multi-repo root without repo name")
	}
}

func TestResolveLocalRepoByRemote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// Child folder name ≠ GitHub repo name; match via origin remote.
	child := filepath.Join(root, "local-name")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	run(child, "init")
	if err := os.WriteFile(filepath.Join(child, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(child, "add", "README")
	run(child, "commit", "-m", "init")
	run(child, "remote", "add", "origin", "git@github.com:acme/other-name.git")

	got, err := ResolveLocalRepo(ctx, root, "acme", "other-name")
	if err != nil || got != child {
		t.Fatalf("got %q err=%v want %q", got, err, child)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"git@github.com:acme/app.git", "acme/app", true},
		{"https://github.com/acme/app", "acme/app", true},
		{"ssh://git@github.com/acme/app.git", "acme/app", true},
		{"https://gitlab.com/acme/app", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := parseGitHubRemote(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%q: got %q ok=%v want %q ok=%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFindOnDiskByUnitID(t *testing.T) {
	data := t.TempDir()
	repo := initTestRepo(t)
	tr, err := Ensure(context.Background(), repo, data, "homeconnect", "1524411722717335604")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := FindOnDiskByUnitID(data, "1524411722717335604")
	if !ok || d.Path != tr.Path || d.Project != "homeconnect" {
		t.Fatalf("got %+v ok=%v want path %q", d, ok, tr.Path)
	}
	if _, ok := FindOnDiskByUnitID(data, "missing"); ok {
		t.Fatal("expected miss")
	}
}

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

func TestRemoveStaleWorktreeMissingFolder(t *testing.T) {
	// Worktree folder deleted outside git still leaves a registration that
	// blocks `git branch -D` with "used by worktree at …".
	repo := initTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()

	tr, err := Ensure(ctx, repo, data, "homeconnect", "1515266754392363009")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tr.Path); err != nil {
		t.Fatal(err)
	}
	// Registration still present until prune.
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+tr.Branch)
	if cmd.Run() != nil {
		t.Fatal("branch should still exist after folder delete")
	}

	if err := Remove(ctx, repo, tr.Path, tr.Branch); err != nil {
		t.Fatalf("Remove with missing folder: %v", err)
	}
	if cmd.Run() == nil {
		t.Fatal("branch should be deleted after Remove")
	}
	// Ensure can recreate the same unit after a stale registration.
	tr2, err := Ensure(ctx, repo, data, "homeconnect", "1515266754392363009")
	if err != nil {
		t.Fatalf("Ensure after stale cleanup: %v", err)
	}
	if !IsRepo(tr2.Path) {
		t.Fatal("recreated worktree not a repo")
	}
	if err := Remove(ctx, repo, tr2.Path, tr2.Branch); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRecoversMissingWorktreeFolder(t *testing.T) {
	repo := initTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()

	tr, err := Ensure(ctx, repo, data, "app", "stale-folder")
	if err != nil {
		t.Fatal(err)
	}
	// Leave branch + commits; only the directory is gone.
	if err := os.WriteFile(filepath.Join(tr.Path, "work.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", tr.Path}, args...)...)
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
	run("add", "work.txt")
	run("commit", "-m", "on branch")

	if err := os.RemoveAll(tr.Path); err != nil {
		t.Fatal(err)
	}

	tr2, err := Ensure(ctx, repo, data, "app", "stale-folder")
	if err != nil {
		t.Fatalf("Ensure should recover: %v", err)
	}
	if tr2.Branch != tr.Branch {
		t.Fatalf("branch=%q want %q", tr2.Branch, tr.Branch)
	}
	// Commit on the managed branch should still be reachable after re-add.
	cmd := exec.Command("git", "-C", tr2.Path, "show", "HEAD:work.txt")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "kept\n" {
		t.Fatalf("lost branch work: err=%v out=%q", err, out)
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

func TestAddDetachedAtCommit(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	// Second commit so we can pin an older SHA.
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "add", "README")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", repo, "commit", "-m", "v2")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	first, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD~1").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(first))
	path := filepath.Join(t.TempDir(), "detached-wt")
	if err := AddDetached(ctx, repo, path, sha); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != sha {
		t.Fatalf("HEAD=%s want %s", head, sha)
	}
	body, err := os.ReadFile(filepath.Join(path, "README"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hi\n" {
		t.Fatalf("want first-commit content, got %q", body)
	}
	// Reuse same path/sha is a no-op.
	if err := AddDetached(ctx, repo, path, sha); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path should be gone: %v", err)
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
