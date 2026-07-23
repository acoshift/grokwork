package bot

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPruneIdleWorktrees(t *testing.T) {
	repo := initIdleTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()

	oldThread := "1001"
	tr, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", oldThread)
	if err != nil {
		t.Fatal(err)
	}

	newThread := "1002"
	trNew, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", newThread)
	if err != nil {
		t.Fatal(err)
	}

	busyThread := "1003"
	trBusy, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", busyThread)
	if err != nil {
		t.Fatal(err)
	}

	orphanThread := "1004"
	trOrphan, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", orphanThread)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(trOrphan.Path, past, past); err != nil {
		t.Fatal(err)
	}

	// Persist sessions with explicit UpdatedAt (store.Set always stamps "now").
	oldAt := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	newAt := time.Now().UTC().Format(time.RFC3339)
	writeSessions(t, data, map[string]sessionstore.Entry{
		oldThread: {
			SessionID:      "s-old",
			Project:        "app",
			Cwd:            tr.Path,
			MainCwd:        repo,
			WorktreeBranch: tr.Branch,
			UpdatedAt:      oldAt,
		},
		newThread: {
			SessionID:      "s-new",
			Project:        "app",
			Cwd:            trNew.Path,
			MainCwd:        repo,
			WorktreeBranch: trNew.Branch,
			UpdatedAt:      newAt,
		},
		busyThread: {
			SessionID:      "s-busy",
			Project:        "app",
			Cwd:            trBusy.Path,
			MainCwd:        repo,
			WorktreeBranch: trBusy.Branch,
			UpdatedAt:      oldAt,
		},
	})

	sessions, err := sessionstore.New(data)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": repo}),
		DataDir:  data,
	}
	b := New(cfg, sessions, nil)

	st := b.stateFor(busyThread)
	st.mu.Lock()
	st.job = &runJob{start: time.Now(), project: "app"}
	st.mu.Unlock()

	removed := b.pruneIdleWorktrees(time.Now(), 30*24*time.Hour)
	if removed < 2 {
		t.Fatalf("removed=%d want >=2 (old session + orphan)", removed)
	}

	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatalf("old worktree still exists: %v", err)
	}
	if _, ok := sessions.Get(oldThread); ok {
		t.Fatal("old session should be deleted")
	}
	if _, err := os.Stat(trNew.Path); err != nil {
		t.Fatalf("fresh worktree should remain: %v", err)
	}
	if _, ok := sessions.Get(newThread); !ok {
		t.Fatal("fresh session should remain")
	}
	if _, err := os.Stat(trBusy.Path); err != nil {
		t.Fatalf("busy worktree should remain: %v", err)
	}
	if _, ok := sessions.Get(busyThread); !ok {
		t.Fatal("busy session should remain")
	}
	if _, err := os.Stat(trOrphan.Path); !os.IsNotExist(err) {
		t.Fatalf("orphan worktree should be removed: %v", err)
	}
}

func TestPruneIdleSkipsWhenTTLZero(t *testing.T) {
	s, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: t.TempDir()}, s, nil)
	if n := b.pruneIdleWorktrees(time.Now(), 0); n != 0 {
		t.Fatalf("n=%d", n)
	}
}

func TestListAndPruneWorktree(t *testing.T) {
	repo := initIdleTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()

	threadID := "2001"
	tr, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", threadID)
	if err != nil {
		t.Fatal(err)
	}
	writeSessions(t, data, map[string]sessionstore.Entry{
		threadID: {
			SessionID:      "s1",
			Project:        "app",
			Cwd:            tr.Path,
			MainCwd:        repo,
			WorktreeBranch: tr.Branch,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		},
	})
	sessions, err := sessionstore.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": repo}),
		DataDir:  data,
	}
	b := New(cfg, sessions, nil)

	list := b.ListWorktrees()
	if len(list) != 1 {
		t.Fatalf("list=%+v", list)
	}
	if list[0].ThreadID != threadID || !list[0].OnDisk || !list[0].HasSession {
		t.Fatalf("info=%+v", list[0])
	}

	if err := b.PruneWorktree(threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatalf("path still exists: %v", err)
	}
	if _, ok := sessions.Get(threadID); ok {
		t.Fatal("session should be gone")
	}
	if n := len(b.ListWorktrees()); n != 0 {
		t.Fatalf("list after prune len=%d", n)
	}

	// Busy refuse
	tr2, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", "2002")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Set("2002", sessionstore.Entry{
		SessionID: "s2", Project: "app", Cwd: tr2.Path, MainCwd: repo, WorktreeBranch: tr2.Branch,
	}); err != nil {
		t.Fatal(err)
	}
	st := b.stateFor("2002")
	st.mu.Lock()
	st.job = &runJob{start: time.Now(), project: "app"}
	st.mu.Unlock()
	if err := b.PruneWorktree("2002"); err == nil {
		t.Fatal("expected busy error")
	}
}

func TestListWorktreesHealsStaleDataDirCwd(t *testing.T) {
	// Sessions saved under an old absolute dataDir (e.g. …/grok-discord/data/…)
	// while worktrees still exist under the new dataDir (…/grokwork/data/…).
	repo := initIdleTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()
	threadID := "rename-1"
	tr, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", threadID)
	if err != nil {
		t.Fatal(err)
	}
	staleCwd := filepath.Join(t.TempDir(), "grok-discord", "data", "worktrees", "app", threadID)
	writeSessions(t, data, map[string]sessionstore.Entry{
		threadID: {
			SessionID:      "s",
			Project:        "app",
			Cwd:            staleCwd,
			MainCwd:        repo,
			WorktreeBranch: tr.Branch,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		},
	})
	sessions, err := sessionstore.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": repo}),
		DataDir:  data,
	}
	b := New(cfg, sessions, nil)

	list := b.ListWorktrees()
	if len(list) != 1 {
		t.Fatalf("list=%+v", list)
	}
	if !list[0].OnDisk {
		t.Fatalf("expected OnDisk after dataDir rename heal: %+v", list[0])
	}
	if list[0].Path != tr.Path {
		t.Fatalf("path=%q want %q", list[0].Path, tr.Path)
	}
	// Session cwd should be rewritten to the live path.
	e, ok := sessions.Get(threadID)
	if !ok || e.Cwd != tr.Path {
		t.Fatalf("session not healed: ok=%v cwd=%q want %q", ok, e.Cwd, tr.Path)
	}
}

func TestPruneIdleNowUsesConfig(t *testing.T) {
	repo := initIdleTestRepo(t)
	data := t.TempDir()
	ctx := context.Background()
	threadID := "3001"
	tr, err := gitworktree.Ensure(ctx, repo, filepath.Join(data, "worktrees"), "app", threadID)
	if err != nil {
		t.Fatal(err)
	}
	oldAt := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	writeSessions(t, data, map[string]sessionstore.Entry{
		threadID: {
			SessionID: "s", Project: "app", Cwd: tr.Path, MainCwd: repo,
			WorktreeBranch: tr.Branch, UpdatedAt: oldAt,
		},
	})
	sessions, err := sessionstore.New(data)
	if err != nil {
		t.Fatal(err)
	}
	days := 30
	cfg := &config.Config{
		Projects:            config.PathProjects(map[string]string{"app": repo}),
		DataDir:             data,
		WorktreeIdleTTLDays: &days,
	}
	b := New(cfg, sessions, nil)
	n, err := b.PruneIdleNow()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d", n)
	}

	zero := 0
	cfg.WorktreeIdleTTLDays = &zero
	if _, err := b.PruneIdleNow(); err == nil {
		t.Fatal("expected disabled error")
	}
}

func TestSessionHasWorktree(t *testing.T) {
	if sessionHasWorktree(sessionstore.Entry{}) {
		t.Fatal("empty")
	}
	if !sessionHasWorktree(sessionstore.Entry{WorktreeBranch: "grok/discord/1"}) {
		t.Fatal("branch")
	}
	if !sessionHasWorktree(sessionstore.Entry{Cwd: "/wt", MainCwd: "/main"}) {
		t.Fatal("cwd pair")
	}
	if sessionHasWorktree(sessionstore.Entry{Cwd: "/main", MainCwd: "/main"}) {
		t.Fatal("same cwd is not worktree")
	}
}

func writeSessions(t *testing.T, dataDir string, entries map[string]sessionstore.Entry) {
	t.Helper()
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func initIdleTestRepo(t *testing.T) string {
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
