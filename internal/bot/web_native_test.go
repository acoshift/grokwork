package bot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/gitworktree"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
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
}

func TestResolveRunCwdWebVsDiscordPrefix(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, proj)
	cfg := &config.Config{
		GrokBin:           "false",
		Projects:          config.PathProjects(map[string]string{"app": proj}),
		Channels:          map[string]string{"ch1": "app"},
		DataDir:           filepath.Join(dir, "data"),
		ConfigPath:        filepath.Join(dir, "config.json"),
		WorktreeIsolation: boolPtr(true),
		MaxTurns:          5,
		TimeoutMs:         5000,
		Yolo:              boolPtr(true),
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	ctx := context.Background()
	pref := projectRef{Name: "app", Cwd: proj}

	// Discord snowflake → grok/discord/
	cwd, branch, err := b.resolveRunCwd(ctx, pref, "1524726013211316294")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "grok/discord/1524726013211316294" {
		t.Fatalf("discord branch=%q cwd=%q", branch, cwd)
	}
	if !strings.Contains(cwd, "1524726013211316294") {
		t.Fatalf("cwd=%q", cwd)
	}

	// Web-native unit id → grok/web/
	webID := gitworktree.NewWebUnitID()
	if err := store.Set(webID, sessionstore.Entry{Project: "app", Origin: SourceWeb}); err != nil {
		t.Fatal(err)
	}
	cwd2, branch2, err := b.resolveRunCwd(ctx, pref, webID)
	if err != nil {
		t.Fatal(err)
	}
	want := gitworktree.WebBranchPrefix + webID
	if branch2 != want {
		t.Fatalf("web branch=%q want %q cwd=%q", branch2, want, cwd2)
	}
	if !gitworktree.IsManagedBranch(branch2) {
		t.Fatal("web branch must be managed")
	}
}
