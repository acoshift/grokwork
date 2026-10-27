package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// applyPRInfo must not invent a turn stamp: the poller Patches every open PR
// session on a short cycle, and thrashing UpdatedAt was the whole point of
// turn-only stamping.
func TestApplyPRInfoPreservesUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	// Web unit id → no Discord card path (nil session is fine).
	threadID := "w_pr_poll_stamp"
	fixed := "2026-01-15T12:00:00Z"
	if err := store.Set(threadID, sessionstore.Entry{
		SessionID: "s1",
		Project:   "app",
		UpdatedAt: fixed,
		LastUser:  "alice",
	}); err != nil {
		t.Fatal(err)
	}
	info := ghpr.Info{
		Number:         7,
		URL:            "https://github.com/acme/app/pull/7",
		Title:          "fix",
		State:          "OPEN",
		Owner:          "acme",
		Repo:           "app",
		Checks:         "✓ 1",
		ReviewDecision: "REVIEW_REQUIRED",
	}
	if err := b.applyPRInfo(nil, threadID, info); err != nil {
		t.Fatalf("applyPRInfo: %v", err)
	}
	got, ok := store.Get(threadID)
	if !ok {
		t.Fatal("session missing")
	}
	if got.UpdatedAt != fixed {
		t.Fatalf("UpdatedAt moved %q → %q (poller must not stamp)", fixed, got.UpdatedAt)
	}
	if got.LastUser != "alice" {
		t.Fatalf("LastUser=%q", got.LastUser)
	}
	got.NormalizePRs()
	if len(got.PRs) != 1 || got.PRs[0].Number != 7 {
		t.Fatalf("PR not upserted: %+v", got.PRs)
	}
}

func TestPreservePRFields(t *testing.T) {
	prev := sessionstore.Entry{
		PRURL:          "https://github.com/o/r/pull/3",
		PRNumber:       3,
		PRState:        "OPEN",
		PRTitle:        "t",
		PRChecks:       "✓ 1",
		PRReview:       "APPROVED",
		PRHeadSHA:      "abc",
		PRIsDraft:      true,
		PRStatusMsgID:  "msg-1",
		CINotifiedSHA:  "abc",
		CIAutoFixCount: 1,
		CIAutoFixSHA:   "abc",
		Goal:           "fix it",
		BriefMsgID:     "brief-1",
		Origin:         SourceWeb,
		CreatedBy:      "web-user-1",
		CreatedByName:  "Web Alice",
		DiscordURL:     "https://discord.com/channels/1/2",
	}
	next := sessionstore.Entry{
		SessionID: "s",
		Project:   "p",
	}
	preservePRFields(&next, prev)
	if next.PRNumber != 3 || next.PRStatusMsgID != "msg-1" || !next.PRIsDraft {
		t.Fatalf("next=%+v", next)
	}
	if next.CINotifiedSHA != "abc" || next.CIAutoFixCount != 1 || next.CIAutoFixSHA != "abc" {
		t.Fatalf("ci fields not preserved: %+v", next)
	}
	if next.Goal != "fix it" || next.BriefMsgID != "brief-1" {
		t.Fatalf("brief fields not preserved: %+v", next)
	}
	if next.Origin != SourceWeb || next.CreatedBy != "web-user-1" || next.CreatedByName != "Web Alice" || next.DiscordURL == "" {
		t.Fatalf("workflow fields not preserved via preservePRFields: %+v", next)
	}
	if next.SessionID != "s" {
		t.Fatalf("clobbered session: %+v", next)
	}
}

func TestEntryPRInfoStatusLines(t *testing.T) {
	e := sessionstore.Entry{
		PRNumber:  9,
		PRURL:     "https://github.com/o/r/pull/9",
		PRState:   "OPEN",
		PRIsDraft: false,
		PRChecks:  "✓ 2",
		PRReview:  "REVIEW_REQUIRED",
	}
	e.NormalizePRs()
	lines := ghpr.FormatMultiStatusLines(entryPRInfos(e))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"#9", "OPEN", "✓ 2", "REVIEW_REQUIRED"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestMultiPRUpsertAndStatus(t *testing.T) {
	e := sessionstore.Entry{SessionID: "s", Project: "p"}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/a/pull/1", Number: 1, State: "OPEN", Owner: "acoshift", Repo: "a",
	})
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/b/pull/2", Number: 2, State: "OPEN", Owner: "acoshift", Repo: "b",
	})
	if len(e.PRs) != 2 {
		t.Fatalf("prs=%d", len(e.PRs))
	}
	if !e.HasOpenPR() || e.AllPRsTerminal() {
		t.Fatal("expected open PRs")
	}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acoshift/a/pull/1", Number: 1, State: "MERGED", Owner: "acoshift", Repo: "a",
	})
	if len(e.OpenPRs()) != 1 {
		t.Fatalf("open=%d", len(e.OpenPRs()))
	}
	lines := ghpr.FormatMultiStatusLines(entryPRInfos(e))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "2 tracked") || !strings.Contains(joined, "acoshift/b#2") {
		t.Fatalf("status=%q", joined)
	}
}

func TestPrRepoDirPrefersWorktree(t *testing.T) {
	// Empty paths → empty (no real git dirs in unit test).
	if got := prRepoDir(sessionstore.Entry{}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestPrViewCwdFallsBackWithoutGitRoot(t *testing.T) {
	// Multi-repo project root: directory exists but is not a git worktree.
	// Poller used to skip these forever (session PR stuck at OPEN after merge).
	root := t.TempDir()
	proj := filepath.Join(root, "monorepo-parent")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nested real-ish layout without initializing git at parent.
	if err := os.MkdirAll(filepath.Join(proj, "apiserver"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"deploys": proj}),
		DataDir:  root,
	}
	b := New(cfg, nil, nil)

	e := sessionstore.Entry{
		Project: "deploys",
		Cwd:     proj,
		MainCwd: proj,
	}
	if got := prRepoDir(e); got != "" {
		t.Fatalf("prRepoDir should be empty for non-git parent, got %q", got)
	}
	got := b.prViewCwd(e)
	if got != proj {
		t.Fatalf("prViewCwd=%q want project path %q", got, proj)
	}

	// Session paths missing → still use configured project path.
	e2 := sessionstore.Entry{Project: "deploys"}
	if got := b.prViewCwd(e2); got != proj {
		t.Fatalf("prViewCwd from config=%q want %q", got, proj)
	}
}

func TestPrViewSelectorPrefersURL(t *testing.T) {
	sel := prViewSelector(sessionstore.TrackedPR{
		Number: 244,
		Owner:  "deploys-app",
		Repo:   "apiserver",
		// No URL — should still build one for non-git cwd polling.
	})
	if sel != "https://github.com/deploys-app/apiserver/pull/244" {
		t.Fatalf("sel=%q", sel)
	}
	sel = prViewSelector(sessionstore.TrackedPR{
		URL:    "https://github.com/deploys-app/apiserver/pull/244",
		Number: 244,
	})
	if sel != "https://github.com/deploys-app/apiserver/pull/244" {
		t.Fatalf("url sel=%q", sel)
	}
}

func TestApplyPRInfoNoOpSkipsStoreWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, nil)
	drainBotOnCleanup(t, b)
	threadID := "w_pr_noop"
	if err := store.Set(threadID, sessionstore.Entry{SessionID: "s1", Project: "app"}); err != nil {
		t.Fatal(err)
	}
	info := ghpr.Info{
		Number: 7, URL: "https://github.com/acme/app/pull/7", Title: "fix",
		State: "OPEN", Owner: "acme", Repo: "app",
		Checks: "✓ 1", ReviewDecision: "REVIEW_REQUIRED", HeadSHA: "abc",
	}
	if err := b.applyPRInfo(nil, threadID, info); err != nil {
		t.Fatal(err)
	}
	rev := store.Rev()
	if err := b.applyPRInfo(nil, threadID, info); err != nil {
		t.Fatal(err)
	}
	if store.Rev() != rev {
		t.Fatalf("unchanged poll rewrote sessions.json: rev %d → %d", rev, store.Rev())
	}
}

func TestShouldImportOpenPRsThrottle(t *testing.T) {
	b := New(&config.Config{DataDir: t.TempDir()}, nil, nil)
	drainBotOnCleanup(t, b)
	now := time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC)
	if !b.shouldImportOpenPRs("initial", now) {
		t.Fatal("initial poll must list GitHub")
	}
	b.notePRImport(now)
	if b.shouldImportOpenPRs("tick", now.Add(prStatusPollInterval)) {
		t.Fatal("90s tick must not re-list every repo")
	}
	if !b.shouldImportOpenPRs("tick", now.Add(prImportInterval)) {
		t.Fatal("import interval elapsed must list again")
	}
	if !b.shouldImportOpenPRs("initial", now.Add(time.Second)) {
		t.Fatal("explicit initial still lists")
	}
}

func TestMaybeHandleCIFailureDebounceSkipsGH(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 7, State: "OPEN",
		URL:     "https://github.com/acme/app/pull/7",
		HeadSHA: "abc", CINotifiedSHA: "abc", Checks: "✗ 1",
	})
	if err := store.Set("w_ci_debounce", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatalf("debounced CI must not exec %s %v", name, args)
		return nil, nil
	})
	b.maybeHandleCIFailure(&discordgo.Session{}, "w_ci_debounce", ghpr.Info{
		Number: 7, State: "OPEN", Owner: "acme", Repo: "app",
		URL: "https://github.com/acme/app/pull/7", HeadSHA: "abc", Checks: "✗ 1",
	})
}

// Regression: cleanup must not run while the job is still held (executeTask path),
// and must run once the thread is idle (finishRun → tryCleanupTerminalPR).
// Terminal cleanup free the worktree but always keep the session so PR links
// and closed state stay on the sessions list/detail (cases and eng units).
// Open (non-closed) cases skip cleanup so reopen-after-ship keeps the worktree.
func TestCleanupWhenAllPRsDoneKeepsSessionAndPRs(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	threadID := "case-thread-1"
	if err := store.Set(threadID, sessionstore.Entry{
		SessionID:      "s-case",
		Project:        "homeconnect",
		Mode:           "case",
		Phase:          sessionstore.PhaseClosed,
		CustomerTitle:  "dashboard.stats timeout",
		Cwd:            filepath.Join(dir, "wt"),
		WorktreeBranch: "grokwork/" + threadID,
		MainCwd:        dir,
		PRNumber:       99,
		PRState:        "MERGED",
		PRURL:          "https://github.com/o/r/pull/99",
		Resolution:     "fixed",
	}); err != nil {
		t.Fatal(err)
	}

	if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	e, ok := store.Get(threadID)
	if !ok {
		t.Fatal("case session deleted after terminal PR cleanup")
	}
	if e.Mode != "case" || e.Phase != sessionstore.PhaseClosed {
		t.Fatalf("case fields lost: mode=%q phase=%q", e.Mode, e.Phase)
	}
	if e.CustomerTitle != "dashboard.stats timeout" {
		t.Fatalf("title=%q", e.CustomerTitle)
	}
	// Worktree pointers cleared so UI does not claim a removed tree.
	if e.Cwd != "" || e.WorktreeBranch != "" {
		t.Fatalf("want worktree cleared, got cwd=%q branch=%q", e.Cwd, e.WorktreeBranch)
	}
	e.NormalizePRs()
	if len(e.PRs) != 1 || e.PRs[0].Number != 99 || e.PRs[0].State != "MERGED" {
		t.Fatalf("case PR not preserved: %+v", e.PRs)
	}
	// Eng (non-case) sessions are also kept with PR metadata.
	fixID := "fix-thread-1"
	if err := store.Set(fixID, sessionstore.Entry{
		SessionID: "s-fix",
		Project:   "p",
		Label:     sessionstore.LabelDone,
		PRNumber:  1,
		PRState:   "MERGED",
		PRURL:     "https://github.com/o/r/pull/1",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/o/r/pull/1", Number: 1, State: "MERGED",
			Title: "ship it", Owner: "o", Repo: "r",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.cleanupWhenAllPRsDone(fixID); err != nil {
		t.Fatalf("fix cleanup: %v", err)
	}
	fe, ok := store.Get(fixID)
	if !ok {
		t.Fatal("eng session deleted after terminal PR cleanup")
	}
	fe.NormalizePRs()
	if len(fe.PRs) != 1 || fe.PRs[0].Number != 1 || fe.PRs[0].State != "MERGED" {
		t.Fatalf("eng PR not preserved: %+v", fe.PRs)
	}
	if fe.Label != sessionstore.LabelDone {
		t.Fatalf("label lost: %q", fe.Label)
	}
}

// Reopened (or otherwise open) cases keep worktrees even when historical PRs are terminal.
func TestCleanupWhenAllPRsDoneSkipsOpenCase(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	threadID := "reopened-case-1"
	wt := filepath.Join(dir, "wt-reopen")
	branch := "grokwork/" + threadID
	if err := store.Set(threadID, sessionstore.Entry{
		SessionID:      "s-reopen",
		Project:        "homeconnect",
		Mode:           "case",
		Phase:          sessionstore.PhaseFixing,
		CustomerTitle:  "bug returned",
		Cwd:            wt,
		WorktreeBranch: branch,
		MainCwd:        dir,
		Label:          sessionstore.LabelInProgress,
		PRNumber:       99,
		PRState:        "MERGED",
		PRURL:          "https://github.com/o/r/pull/99",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/o/r/pull/99", Number: 99, State: "MERGED",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.cleanupWhenAllPRsDone(threadID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	e, ok := store.Get(threadID)
	if !ok {
		t.Fatal("session missing")
	}
	if e.Cwd != wt || e.WorktreeBranch != branch {
		t.Fatalf("open case worktree must survive terminal cleanup: cwd=%q branch=%q", e.Cwd, e.WorktreeBranch)
	}
	if e.Phase != sessionstore.PhaseFixing {
		t.Fatalf("phase=%q", e.Phase)
	}
}

func TestTryCleanupTerminalPRDefersWhenBusy(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	threadID := "thread-1"
	if err := store.Set(threadID, sessionstore.Entry{
		SessionID: "s1",
		Project:   "p",
		PRNumber:  7,
		PRState:   "MERGED",
		PRURL:     "https://github.com/o/r/pull/7",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate active run (same as during executeTask / refreshPRAfterTask).
	job := &runJob{cancel: func() {}, start: time.Now(), project: "p"}
	if _, _, err := b.claimOrEnqueue(threadID, job, taskItem{threadID: threadID}); err != nil {
		t.Fatal(err)
	}
	b.tryCleanupTerminalPR(threadID)
	if _, ok := store.Get(threadID); !ok {
		t.Fatal("session deleted while busy — eager cleanup raced the active job")
	}

	// Release job (finishRun with empty queue).
	if _, ok := b.finishRun(threadID); ok {
		t.Fatal("expected no queued next")
	}
	b.tryCleanupTerminalPR(threadID)
	e, ok := store.Get(threadID)
	if !ok {
		t.Fatal("session should be kept after idle terminal cleanup")
	}
	if e.PRNumber != 7 || e.PRState != "MERGED" {
		t.Fatalf("PR fields lost after cleanup: num=%d state=%q", e.PRNumber, e.PRState)
	}
}

// A reply that cites an upstream library PR must not bind it. A URL in the
// project's own catalog, in the same reply, still binds.
func TestRefreshPRAfterTaskSkipsLibraryURL(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "Acme", "App")
	const id = "w_lib_url"
	if err := store.Set(id, sessionstore.Entry{
		Project: "app", SessionID: "s", UpdatedAt: "2026-10-26T23:44:48Z",
	}); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "oven-sh/bun") {
			t.Fatalf("library PR must not be viewed: %s", joined)
		}
		if strings.Contains(joined, "pr checks") {
			return []byte(`[]`), nil
		}
		if strings.Contains(joined, "acme/app/pull/40") {
			return []byte(`{"number":40,"url":"https://github.com/acme/app/pull/40","title":"bump","state":"OPEN","isDraft":false,"headRefOid":"abc","headRefName":"bun-1.4.4"}`), nil
		}
		t.Fatalf("unexpected gh: %s", joined)
		return nil, nil
	})
	reply := "Those flags are only in https://github.com/oven-sh/bun/pull/45631.\n\nhttps://github.com/acme/app/pull/40"
	b.refreshPRAfterTask(nil, id, "", "", reply)
	e, ok := store.Get(id)
	if !ok {
		t.Fatal("session missing")
	}
	e.NormalizePRs()
	if len(e.PRs) != 1 || e.PRs[0].Number != 40 || !strings.EqualFold(e.PRs[0].Repo, "app") {
		t.Fatalf("prs=%+v", e.PRs)
	}
	if e.PRNumber != 40 {
		t.Fatalf("legacy number=%d", e.PRNumber)
	}
	if e.UpdatedAt != "2026-10-26T23:44:48Z" {
		t.Fatalf("UpdatedAt=%q", e.UpdatedAt)
	}
}

// An already-bound library PR is removed on the next poll. A catalog PR and a
// number-only legacy row stay. gh is not asked about the library PR.
func TestPollDropsTrackedLibraryPR(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")
	e := sessionstore.Entry{Project: "app", SessionID: "s", UpdatedAt: "2026-10-26T23:44:48Z"}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/oven-sh/bun/pull/45631", Number: 45631, State: "OPEN",
		Owner: "oven-sh", Repo: "bun", Title: "aot",
	})
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acme/app/pull/40", Number: 40, State: "MERGED",
		Owner: "acme", Repo: "app", Title: "bump",
	})
	e.UpsertPR(sessionstore.TrackedPR{Number: 3, State: "MERGED"})
	if err := store.Set("w_lib_bound", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatalf("gh called: %s", strings.Join(args, " "))
		return nil, nil
	})
	b.pollPRStatuses(nil)
	got, ok := store.Get("w_lib_bound")
	if !ok {
		t.Fatal("session missing")
	}
	got.NormalizePRs()
	if len(got.PRs) != 2 || got.PRs[0].Number != 40 || got.PRs[1].Number != 3 {
		t.Fatalf("prs=%+v", got.PRs)
	}
	if got.UpdatedAt != "2026-10-26T23:44:48Z" {
		t.Fatalf("UpdatedAt=%q", got.UpdatedAt)
	}
}

// No configured catalog and no discoverable remote: do not treat every tracked
// PR as foreign. Projects that learn their repo from origin depend on this.
func TestPollKeepsPRWhenCatalogUnknown(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	e := sessionstore.Entry{Project: "app", SessionID: "s"}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acme/app/pull/7", Number: 7, State: "OPEN",
		Owner: "acme", Repo: "app", Title: "fix",
	})
	if err := store.Set("w_nocat", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "pr checks") {
			return []byte(`[]`), nil
		}
		if strings.Contains(joined, "acme/app/pull/7") {
			return []byte(`{"number":7,"url":"https://github.com/acme/app/pull/7","title":"fix","state":"OPEN","headRefOid":"abc","headRefName":"feat"}`), nil
		}
		t.Fatalf("unexpected gh: %s", joined)
		return nil, nil
	})
	b.pollPRStatuses(nil)
	got, ok := store.Get("w_nocat")
	if !ok {
		t.Fatal("session missing")
	}
	got.NormalizePRs()
	if len(got.PRs) != 1 || got.PRs[0].Number != 7 || got.PRs[0].Repo != "app" {
		t.Fatalf("prs=%+v", got.PRs)
	}
}

// Same GitHub owner, different repo: a platform checkout still tracks
// hah-webadmin. Only a different owner (the library) is removed.
func TestPollKeepsSameOwnerSiblingRepo(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")
	e := sessionstore.Entry{Project: "app", SessionID: "s"}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/acme/lib/pull/8", Number: 8, State: "MERGED",
		Owner: "acme", Repo: "lib", Title: "sibling",
	})
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/oven-sh/bun/pull/45631", Number: 45631, State: "OPEN",
		Owner: "oven-sh", Repo: "bun", Title: "aot",
	})
	if err := store.Set("w_sibling", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatalf("gh called: %s", strings.Join(args, " "))
		return nil, nil
	})
	b.pollPRStatuses(nil)
	got, ok := store.Get("w_sibling")
	if !ok {
		t.Fatal("session missing")
	}
	got.NormalizePRs()
	if len(got.PRs) != 1 || got.PRs[0].Number != 8 || got.PRs[0].Repo != "lib" {
		t.Fatalf("prs=%+v", got.PRs)
	}
}

func TestApplyPRInfoIgnoresOffCatalog(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")
	if err := store.Set("w_apply_lib", sessionstore.Entry{Project: "app", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	info := ghpr.Info{
		Number: 45631, URL: "https://github.com/oven-sh/bun/pull/45631",
		Title: "aot", State: "OPEN", Owner: "oven-sh", Repo: "bun",
	}
	if err := b.applyPRInfo(nil, "w_apply_lib", info); err != nil {
		t.Fatal(err)
	}
	e, ok := store.Get("w_apply_lib")
	if !ok {
		t.Fatal("session missing")
	}
	e.NormalizePRs()
	if e.HasAnyPR() {
		t.Fatalf("library PR bound: %+v", e.PRs)
	}
}
