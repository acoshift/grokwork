package bot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestIdleFetchSkipsDisabledProject(t *testing.T) {
	livePath, liveTip := checkoutBehindRemote(t)
	quietPath, _ := checkoutBehindRemote(t)
	quietBefore := gitRev(t, quietPath, "refs/remotes/origin/main")

	mins := 1
	dir := t.TempDir()
	cfg := &config.Config{
		GrokBin: "false",
		Projects: config.ProjectsMap{
			"live":  {Path: livePath, RepoFetchIntervalMinutes: &mins},
			"quiet": {Path: quietPath, RepoFetchIntervalMinutes: &mins, Disabled: true},
		},
		DataDir:    filepath.Join(dir, "data"),
		ConfigPath: filepath.Join(dir, "config.json"),
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
	drainBotOnCleanup(t, b)

	b.runIdleRepoFetchCycle("test")
	if got := gitRev(t, livePath, "refs/remotes/origin/main"); got != liveTip {
		t.Fatalf("enabled origin/main=%s want %s", got, liveTip)
	}
	if got := gitRev(t, quietPath, "refs/remotes/origin/main"); got != quietBefore {
		t.Fatalf("disabled origin/main moved from %s to %s", quietBefore, got)
	}

	shared, sharedTip := checkoutBehindRemote(t)
	cfg.Projects["off"] = config.ProjectConfig{Path: shared, RepoFetchIntervalMinutes: &mins, Disabled: true}
	cfg.Projects["on"] = config.ProjectConfig{Path: shared, RepoFetchIntervalMinutes: &mins}
	b.runIdleRepoFetchCycle("test-shared")
	if got := gitRev(t, shared, "refs/remotes/origin/main"); got != sharedTip {
		t.Fatalf("shared origin/main=%s want %s", got, sharedTip)
	}
	if got := gitRev(t, quietPath, "refs/remotes/origin/main"); got != quietBefore {
		t.Fatalf("disabled origin/main moved during shared cycle from %s to %s", quietBefore, got)
	}
}

func TestPRSyncSkipsDisabledProject(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	b.cfg.Projects["quiet"] = config.ProjectConfig{Path: t.TempDir(), Disabled: true}
	withProjectGitHub(t, b.cfg, "app", "acme", "live")
	withProjectGitHub(t, b.cfg, "quiet", "acme", "quiet")

	const (
		liveURL  = "https://github.com/acme/live/pull/7"
		quietURL = "https://github.com/acme/quiet/pull/4"
	)
	live := sessionstore.Entry{Project: "app", SessionID: "s-live"}
	live.UpsertPR(sessionstore.TrackedPR{
		URL: liveURL, Number: 7, State: "OPEN", Title: "stale live",
		HeadSHA: "oldlive", HeadRef: "feat-live", Owner: "acme", Repo: "live",
	})
	quiet := sessionstore.Entry{Project: "quiet", SessionID: "s-quiet"}
	quiet.UpsertPR(sessionstore.TrackedPR{
		URL: quietURL, Number: 4, State: "OPEN", Title: "stale quiet",
		HeadSHA: "oldquiet", HeadRef: "feat-quiet", Owner: "acme", Repo: "quiet",
	})
	if err := store.Set("live-unit", live); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("quiet-unit", quiet); err != nil {
		t.Fatal(err)
	}

	var calls []string
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		if strings.Contains(joined, "acme/quiet") || strings.Contains(joined, "quiet/pull") {
			t.Errorf("gh touched the disabled project: %s", joined)
		}
		switch {
		case strings.HasPrefix(joined, "pr list") && strings.Contains(joined, "--repo acme/live"):
			return []byte(`[
				{"number":7,"url":"` + liveURL + `","title":"stale live","state":"OPEN",
				 "author":{"login":"zoe"},"headRefOid":"oldlive","headRefName":"feat-live"},
				{"number":99,"url":"https://github.com/acme/live/pull/99","title":"Human opened this",
				 "state":"OPEN","author":{"login":"zoe"},"headRefOid":"hhh","headRefName":"human-branch"}
			]`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[]`), nil
		case strings.HasPrefix(joined, "pr view") && strings.Contains(joined, liveURL):
			return []byte(`{"number":7,"url":"` + liveURL + `","title":"fresh live","state":"OPEN","isDraft":false,"headRefOid":"newlive","headRefName":"feat-live"}`), nil
		case strings.HasPrefix(joined, "pr view") && strings.Contains(joined, "/pull/99"):
			return []byte(`{"number":99,"url":"https://github.com/acme/live/pull/99","title":"Human opened this","state":"OPEN","isDraft":false,"headRefOid":"hhh","headRefName":"human-branch"}`), nil
		default:
			t.Fatalf("unexpected gh: %s", joined)
			return nil, nil
		}
	})

	if n := b.importOpenGitHubPRs(); n != 1 {
		t.Fatalf("imported=%d want 1 (the enabled project's new PR only)", n)
	}
	b.pollPRStatuses(nil)

	got, ok := store.Get("live-unit")
	if !ok {
		t.Fatal("live session missing")
	}
	got.NormalizePRs()
	if len(got.PRs) != 1 || got.PRs[0].Title != "fresh live" || got.PRs[0].HeadSHA != "newlive" {
		t.Fatalf("enabled PR not refreshed: %+v", got.PRs)
	}
	q, ok := store.Get("quiet-unit")
	if !ok {
		t.Fatal("quiet session missing")
	}
	q.NormalizePRs()
	if len(q.PRs) != 1 || q.PRs[0].Title != "stale quiet" || q.PRs[0].HeadSHA != "oldquiet" {
		t.Fatalf("disabled PR updated from GitHub: %+v", q.PRs)
	}
	for _, listed := range store.List() {
		if listed.Entry.Project == "quiet" && listed.ThreadID != "quiet-unit" {
			t.Fatalf("disabled project gained session %s", listed.ThreadID)
		}
	}
	if !sawGH(calls, "pr list", "--repo acme/live") {
		t.Fatalf("enabled repo was not listed: %v", calls)
	}
	if sawGH(calls, "acme/quiet", "") {
		t.Fatalf("disabled repo was queried: %v", calls)
	}

	pc := b.cfg.Projects["quiet"]
	pc.Disabled = false
	b.cfg.Projects["quiet"] = pc
	calls = nil
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		switch {
		case strings.HasPrefix(joined, "pr list") && strings.Contains(joined, "--repo acme/quiet"):
			return []byte(`[{"number":88,"url":"https://github.com/acme/quiet/pull/88","title":"now listed","state":"OPEN","author":{"login":"zoe"},"headRefOid":"qqq","headRefName":"human-quiet"}]`), nil
		case strings.HasPrefix(joined, "pr list"):
			return []byte(`[]`), nil
		default:
			t.Fatalf("unexpected gh: %s", joined)
			return nil, nil
		}
	})
	if n := b.importOpenGitHubPRs(); n != 1 {
		t.Fatalf("import after clear=%d want 1", n)
	}
	if !sawGH(calls, "pr list", "--repo acme/quiet") {
		t.Fatalf("cleared project was not listed: %v", calls)
	}
	if len(b.FindPRSessions("quiet", "acme", "quiet", 88)) != 1 {
		t.Fatal("cleared project did not import the open PR")
	}
}

func sawGH(calls []string, parts ...string) bool {
	for _, call := range calls {
		ok := true
		for _, part := range parts {
			if part == "" {
				continue
			}
			if !strings.Contains(call, part) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// checkoutBehindRemote is a local clone whose origin/main is one commit behind a bare remote.
func checkoutBehindRemote(t *testing.T) (checkout, tip string) {
	t.Helper()
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	seed := t.TempDir()
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "a.txt")
	runGit(t, seed, "commit", "-m", "v1")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	checkout = t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "clone", remote, checkout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "a.txt")
	runGit(t, seed, "commit", "-m", "v2")
	runGit(t, seed, "push", "origin", "main")

	tip = gitRev(t, remote, "refs/heads/main")
	if have := gitRev(t, checkout, "refs/remotes/origin/main"); have == tip {
		t.Fatal("clone already has the new tip")
	}
	return checkout, tip
}

func gitRev(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// TestPollSkipsGitHubOnDisabledTerminalCleanup drives pollPRStatuses for a
// disabled project whose PRs are already terminal and whose worktree is still
// pending. branchPRTerminal execs gh itself and ignores SetGHRunner, and an
// open case re-enters this path every poll, so the assertion is a PATH shim.
func TestPollSkipsGitHubOnDisabledTerminalCleanup(t *testing.T) {
	calls := ghShimOnPath(t)

	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-m", "init")
	const (
		mergedID     = "quiet-merged"
		caseID       = "quiet-case"
		mergedBranch = "grokwork/quiet-merged"
		caseBranch   = "grokwork/quiet-case"
	)
	runGit(t, repo, "branch", mergedBranch)
	runGit(t, repo, "branch", caseBranch)

	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	b.cfg.Projects["quiet"] = config.ProjectConfig{Path: repo, Disabled: true}
	b.cfg.WebAuth = &config.WebAuthConfig{
		Enabled:  true,
		Features: config.WebAuthFeatures{GitHubWrites: true},
	}

	merged := sessionstore.Entry{
		Project: "quiet", SessionID: "s-merged",
		MainCwd: repo, WorktreeBranch: mergedBranch,
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/quiet/pull/3", Number: 3, State: "MERGED",
			Title: "done", HeadSHA: "abc", HeadRef: mergedBranch, Owner: "acme", Repo: "quiet",
		}},
		Issues: []sessionstore.TrackedIssue{{
			Number: 9, Owner: "acme", Repo: "quiet",
			URL: "https://github.com/acme/quiet/issues/9",
		}},
	}
	openCase := merged
	openCase.SessionID = "s-case"
	openCase.Mode = "case"
	openCase.Phase = sessionstore.PhaseFixing
	openCase.WorktreeBranch = caseBranch
	openCase.PRs = []sessionstore.TrackedPR{{
		URL: "https://github.com/acme/quiet/pull/4", Number: 4, State: "MERGED",
		Title: "case done", HeadSHA: "def", HeadRef: caseBranch, Owner: "acme", Repo: "quiet",
	}}
	if err := store.Set(mergedID, merged); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(caseID, openCase); err != nil {
		t.Fatal(err)
	}

	b.pollPRStatuses(nil)
	if got := readGHCalls(t, calls); got != "" {
		t.Fatalf("disabled terminal cleanup invoked gh:\n%s", got)
	}

	got, ok := store.Get(mergedID)
	if !ok {
		t.Fatal("merged session missing")
	}
	if got.Cwd != "" || got.WorktreeBranch != "" {
		t.Fatalf("local cleanup left worktree pointers: cwd=%q branch=%q", got.Cwd, got.WorktreeBranch)
	}
	if localBranchExists(t, repo, mergedBranch) {
		t.Fatal("local managed branch was not removed")
	}

	kept, ok := store.Get(caseID)
	if !ok {
		t.Fatal("case session missing")
	}
	if kept.WorktreeBranch != caseBranch {
		t.Fatalf("open case worktree cleared: %q", kept.WorktreeBranch)
	}
	if !localBranchExists(t, repo, caseBranch) {
		t.Fatal("open case branch was deleted")
	}

	// The open case stays pending, so the next poll walks cleanup again.
	b.pollPRStatuses(nil)
	if got := readGHCalls(t, calls); got != "" {
		t.Fatalf("second poll invoked gh:\n%s", got)
	}
	if !localBranchExists(t, repo, caseBranch) {
		t.Fatal("second poll deleted the open case branch")
	}
}

// ghShimOnPath puts a fake gh ahead of PATH. branchPRTerminal and the default
// gh runner both exec "gh" by name, so this sees calls SetGHRunner cannot.
func ghShimOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func readGHCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func localBranchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}
