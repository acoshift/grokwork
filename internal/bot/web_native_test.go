package bot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
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

// Production DiscordReady is true after Register even when REST is down.
// CreateWorkflowThread failure must fall back to web-native (advisor major 1).
func TestStartFixCreateThreadFailFallsBackWebNative(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	// Simulate "gateway session present" (Register) while thread API fails.
	s, err := discordgo.New("Bot fake-token")
	if err != nil {
		t.Fatal(err)
	}
	b.setDiscord(s)
	b.threadAPI = &fakeThreadAPI{failStart: fmt.Errorf("discord api outage")}

	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 42,
		Title: "outage", Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatalf("expected web-native fallback, got %v", err)
	}
	if !res.Created || !gitworktree.IsWebUnitID(res.ThreadID) {
		t.Fatalf("want web-native unit, got %+v", res)
	}
	if res.DiscordURL != "" {
		t.Fatalf("web-native should not have Discord URL: %+v", res)
	}
	waitHistory(t, b, res.ThreadID, 1)
}

// PR discovery must bind session PRs without a Discord session (advisor major 2).
func TestRefreshPRAfterTaskNilSessionBinds(t *testing.T) {
	b, _ := testFixBot(t)
	webID := gitworktree.NewWebUnitID()
	if err := b.sessions.Set(webID, sessionstore.Entry{
		Project: "app", Origin: SourceWeb,
	}); err != nil {
		t.Fatal(err)
	}
	// Inject gh so View-by-URL works without network.
	// refreshPRAfterTask → discoverPRInfos → ghpr.View uses gh binary.
	// Use applyPRInfo path directly with nil session to prove bind.
	info := ghpr.Info{
		Number: 7, URL: "https://github.com/acme/app/pull/7",
		Title: "fix", State: "OPEN", Owner: "acme", Repo: "app",
	}
	if err := b.applyPRInfo(nil, webID, info); err != nil {
		t.Fatal(err)
	}
	e, ok := b.sessions.Get(webID)
	if !ok {
		t.Fatal("session missing")
	}
	e.NormalizePRs()
	if !e.HasAnyPR() || e.PRs[0].Number != 7 {
		t.Fatalf("PR not bound: %+v", e.PRs)
	}
	// Card path must not error-spam: web unit + nil s → no Discord post.
	id, err := b.upsertPRStatusMessage(nil, webID, "", "card body")
	if err != nil {
		t.Fatalf("web unit card skip should succeed: %v", err)
	}
	if id != "" {
		t.Fatalf("unexpected msg id %q", id)
	}
}

func TestPollPRStatusesNilSessionStillCleansTerminal(t *testing.T) {
	b, _ := testFixBot(t)
	webID := gitworktree.NewWebUnitID()
	e := sessionstore.Entry{
		Project: "app", Origin: SourceWeb,
		WorktreeBranch: gitworktree.BranchNameForUnit(webID),
	}
	e.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 9, State: "MERGED",
		URL: "https://github.com/acme/app/pull/9",
	})
	if err := b.sessions.Set(webID, e); err != nil {
		t.Fatal(err)
	}
	// s == nil must not early-return before terminal cleanup.
	stats := b.pollPRStatuses(nil)
	if stats.Sessions < 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if _, ok := b.sessions.Get(webID); ok {
		t.Fatal("expected terminal session cleanup without Discord")
	}
}
