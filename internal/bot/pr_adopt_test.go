package bot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestTrackedPRToAdopt(t *testing.T) {
	t.Parallel()
	imported := sessionstore.Entry{
		Project:     "app",
		SessionKind: sessionstore.SessionKindImportedPR,
	}
	imported.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 12, State: "OPEN", HeadRef: "feat-x",
		URL: "https://github.com/acme/app/pull/12",
	})
	pr, branch, ok := trackedPRToAdopt(imported, "")
	if !ok || branch != "feat-x" || pr.Number != 12 {
		t.Fatalf("imported: ok=%v branch=%q pr=%d", ok, branch, pr.Number)
	}

	// Kind already cleared after first run; WorktreeBranch is the adopted head.
	cleared := imported
	cleared.SessionKind = ""
	cleared.WorktreeBranch = "feat-x"
	_, branch, ok = trackedPRToAdopt(cleared, "")
	if !ok || branch != "feat-x" {
		t.Fatalf("cleared kind: ok=%v branch=%q", ok, branch)
	}

	managed := imported
	managed.SessionKind = ""
	managed.WorktreeBranch = "grok/web/w_1"
	if _, _, ok := trackedPRToAdopt(managed, ""); ok {
		t.Fatal("managed worktree must not adopt")
	}

	review := imported
	review.SessionKind = sessionstore.SessionKindPRReview
	if _, _, ok := trackedPRToAdopt(review, ""); ok {
		t.Fatal("PR review must not adopt")
	}

	primaryHead := imported
	primaryHead.PRs[0].HeadRef = "main"
	if _, _, ok := trackedPRToAdopt(primaryHead, ""); ok {
		t.Fatal("primary head must not adopt")
	}

	none := sessionstore.Entry{Project: "app"}
	if _, _, ok := trackedPRToAdopt(none, ""); ok {
		t.Fatal("no PR must not adopt")
	}
}

func TestExistingPRForPrompt(t *testing.T) {
	t.Parallel()
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	pr, ok := existingPRForPrompt(e)
	if !ok || pr.Number != 9 {
		t.Fatalf("%v %+v", ok, pr)
	}
	e.SessionKind = sessionstore.SessionKindPRReview
	if _, ok := existingPRForPrompt(e); ok {
		t.Fatal("review units do not continue-PR")
	}
}

func TestResolveRunCwdAdoptsImportedPRHead(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, proj)
	primary := gitOutTrim(t, proj, "rev-parse", "--abbrev-ref", "HEAD")
	runGitIn(t, proj, "checkout", "-b", "feat-x")
	if err := os.WriteFile(filepath.Join(proj, "pr.txt"), []byte("imported\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, proj, "add", "pr.txt")
	runGitIn(t, proj, "commit", "-m", "pr")
	runGitIn(t, proj, "checkout", primary)

	cfg := &config.Config{
		GrokBin:           "false",
		Projects:          config.PathProjects(map[string]string{"app": proj}),
		Channels:          map[string]string{"ch1": "app"},
		DataDir:           filepath.Join(dir, "data"),
		ConfigPath:        filepath.Join(dir, "config.json"),
		WorktreeIsolation: new(true),
		MaxTurns:          5,
		TimeoutMs:         5000,
		Yolo:              new(true),
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
	unit := gitworktree.NewWebUnitID()
	e := sessionstore.Entry{
		Project:     "app",
		SessionKind: sessionstore.SessionKindImportedPR,
		Origin:      SourceWeb,
	}
	e.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 12, State: "OPEN", HeadRef: "feat-x",
		URL: "https://github.com/acme/app/pull/12",
	})
	if err := store.Set(unit, e); err != nil {
		t.Fatal(err)
	}

	cwd, branch, err := b.resolveRunCwd(t.Context(), projectRef{Name: "app", Cwd: proj}, unit)
	if err != nil {
		t.Fatal(err)
	}
	if branch != "feat-x" {
		t.Fatalf("branch=%q want feat-x (not a managed grok/web/ branch)", branch)
	}
	if gitworktree.IsManagedBranch(branch) {
		t.Fatal("imported PR must not mint a managed branch")
	}
	body, err := os.ReadFile(filepath.Join(cwd, "pr.txt"))
	if err != nil || string(body) != "imported\n" {
		t.Fatalf("worktree not on PR head: %q err=%v", body, err)
	}
	got, ok := store.Get(unit)
	if !ok || got.WorktreeBranch != "feat-x" {
		t.Fatalf("session WorktreeBranch=%q", got.WorktreeBranch)
	}

	// After kind is cleared, a later turn still reuses the PR head.
	b.clearImportedPRKind(unit)
	cwd2, branch2, err := b.resolveRunCwd(t.Context(), projectRef{Name: "app", Cwd: proj}, unit)
	if err != nil {
		t.Fatal(err)
	}
	if branch2 != "feat-x" || cwd2 != cwd {
		t.Fatalf("reuse branch=%q cwd=%q", branch2, cwd2)
	}
}

func TestEnsureShipModeImportedPRStaysPR(t *testing.T) {
	b, store := testBotSessions(t)
	on := true
	pc := b.cfg.Projects["app"]
	pc.DirectToPrimary = &on
	b.cfg.Projects["app"] = pc

	e := sessionstore.Entry{Project: "app", SessionKind: sessionstore.SessionKindImportedPR}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 12, State: "OPEN", HeadRef: "feat-x"})
	if err := store.Set("w_imp_ship", e); err != nil {
		t.Fatal(err)
	}
	if got := b.ensureShipMode("w_imp_ship", "app"); got != sessionstore.ShipModePR {
		t.Fatalf("imported shipMode=%q want pr", got)
	}
	got, _ := store.Get("w_imp_ship")
	if got.ShipMode != sessionstore.ShipModePR {
		t.Fatalf("stamped %q", got.ShipMode)
	}

	// Fresh session with no PR still follows the project default.
	if err := store.Set("w_new", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	if got := b.ensureShipMode("w_new", "app"); got != sessionstore.ShipModeDirect {
		t.Fatalf("empty session shipMode=%q want direct", got)
	}
}

func TestSnapshotPolicyImportedPRAllowsPR(t *testing.T) {
	b, store := testBotSessions(t)
	on := true
	pc := b.cfg.Projects["app"]
	pc.DirectToPrimary = &on
	b.cfg.Projects["app"] = pc
	e := sessionstore.Entry{Project: "app", SessionKind: sessionstore.SessionKindImportedPR}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 3, State: "OPEN", HeadRef: "feat-x"})
	if err := store.Set("w_imp_pol", e); err != nil {
		t.Fatal(err)
	}
	item := taskItem{threadID: "w_imp_pol", parsed: Parsed{Kind: KindTask}, actor: Actor{ID: "u"}}
	b.snapshotPolicyOntoItem(&item, "app")
	if !item.snapAllowPR {
		t.Fatalf("imported PR session on direct-to-primary must AllowPR: allowPR=%v allowDirect=%v", item.snapAllowPR, item.snapAllowDirect)
	}
	if item.snapAllowDirect {
		t.Fatal("must not snapshot direct-ship for an open imported PR")
	}
}

func TestRemoteWorkPromptContinuePR(t *testing.T) {
	pr := sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 12,
		URL: "https://github.com/acme/app/pull/12", HeadRef: "feat-x",
	}
	p := remoteWorkPromptContinuePR("feat-x", "main", pr)
	for _, want := range []string{
		"Branch: feat-x",
		"acme/app#12",
		"https://github.com/acme/app/pull/12",
		"Do NOT open a new pull request",
		"`gh pr create` is forbidden",
		"Push updates the existing PR",
		"Do not merge",
		"SCRUTINIZE_VERDICT:",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "4. Open a pull request with `gh pr create`") {
		t.Fatalf("must not instruct opening a PR:\n%s", p)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
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

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func gitOutTrim(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, dir, args...))
}
