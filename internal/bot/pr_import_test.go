package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func withProjectGitHub(t *testing.T, cfg *config.Config, project, owner, repo string) {
	t.Helper()
	pc := cfg.Projects[project]
	pc.GitHub = &config.ProjectGitHubConfig{Repos: []config.GitHubRepoRef{{Owner: owner, Repo: repo}}}
	cfg.Projects[project] = pc
}

func TestImportOpenGitHubPRs(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")

	var lists int
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "pr list") {
			t.Fatalf("unexpected gh: %v", args)
		}
		if !strings.Contains(joined, "--repo acme/app") {
			t.Fatalf("missing --repo: %v", args)
		}
		lists++
		return []byte(`[
			{"number":12,"url":"https://github.com/acme/app/pull/12","title":"Human opened this",
			 "state":"OPEN","isDraft":false,"author":{"login":"zoe"},"reviewDecision":"REVIEW_REQUIRED",
			 "headRefOid":"abc","headRefName":"feat-x","updatedAt":"2026-08-30T10:00:00Z"},
			{"number":13,"url":"https://github.com/acme/app/pull/13","title":"Grokwork in flight",
			 "state":"OPEN","isDraft":false,"author":{"login":"bot"},"headRefOid":"def",
			 "headRefName":"grokwork/123","updatedAt":"2026-08-30T10:00:00Z"}
		]`), nil
	})

	n := b.importOpenGitHubPRs()
	if n != 1 {
		t.Fatalf("imported=%d want 1", n)
	}
	if lists != 1 {
		t.Fatalf("list calls=%d", lists)
	}

	hits := b.FindPRSessions("app", "acme", "app", 12)
	if len(hits) != 1 {
		t.Fatalf("FindPRSessions #12: %+v", hits)
	}
	e, ok := store.Get(hits[0].ThreadID)
	if !ok {
		t.Fatal("imported unit missing")
	}
	if !e.IsImportedPR() {
		t.Fatalf("SessionKind=%q", e.SessionKind)
	}
	if e.WorktreeBranch != "" || e.Cwd != "" || e.SessionID != "" || e.Agent != "" || e.OwnerID != "" {
		t.Fatalf("shell must not seed worktree/cli/owner: %+v", e)
	}
	if e.OwnerName != "zoe" || e.Goal != "Human opened this" {
		t.Fatalf("goal/author: goal=%q owner=%q", e.Goal, e.OwnerName)
	}
	if e.UpdatedAt == "" {
		t.Fatal("UpdatedAt must be stamped on import")
	}
	e.NormalizePRs()
	if len(e.PRs) != 1 || e.PRs[0].Number != 12 || e.PRs[0].HeadRef != "feat-x" {
		t.Fatalf("PR=%+v", e.PRs)
	}

	if len(b.FindPRSessions("app", "acme", "app", 13)) != 0 {
		t.Fatal("managed-branch PR must not import")
	}

	n = b.importOpenGitHubPRs()
	if n != 0 {
		t.Fatalf("second cycle imported=%d", n)
	}
	if store.Count() != 1 {
		t.Fatalf("duplicate shells: count=%d", store.Count())
	}
}

func TestImportSkipsAlreadyBound(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 12, State: "OPEN"})
	if err := store.Set("already", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`[{"number":12,"url":"https://github.com/acme/app/pull/12","title":"X","state":"OPEN",
			"author":{"login":"zoe"},"headRefName":"feat"}]`), nil
	})
	if n := b.importOpenGitHubPRs(); n != 0 {
		t.Fatalf("imported=%d", n)
	}
	if store.Count() != 1 {
		t.Fatalf("count=%d", store.Count())
	}
}

func TestFindByPRIncludesImported(t *testing.T) {
	b, store := testBotSessions(t)
	imp := sessionstore.Entry{
		Project: "app", SessionKind: sessionstore.SessionKindImportedPR, Goal: "Human PR",
	}
	imp.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := store.Set("pr-import", imp); err != nil {
		t.Fatal(err)
	}
	hits := b.FindByPR("app", "acme", "app", 9, false)
	if len(hits) != 1 || hits[0].ThreadID != "pr-import" {
		t.Fatalf("%+v", hits)
	}
}

func TestMaybeHandleCIFailureSkipsImported(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	on := true
	b.cfg.AutoFixCI = &on
	started := 0
	b.startTaskHook = func(StartTaskOpts) { started++ }

	e := sessionstore.Entry{
		Project:     "app",
		SessionKind: sessionstore.SessionKindImportedPR,
		Cwd:         t.TempDir(), // even if this looked like a repo, import must not auto-fix
	}
	e.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "app", Number: 12, State: "OPEN",
		URL: "https://github.com/acme/app/pull/12", Checks: "✗ 1", HeadSHA: "deadbeef",
	})
	if err := store.Set("w_import_ci", e); err != nil {
		t.Fatal(err)
	}
	b.maybeHandleCIFailure(&discordgo.Session{}, "w_import_ci", ghpr.Info{
		Number: 12, State: "OPEN", Owner: "acme", Repo: "app",
		URL: "https://github.com/acme/app/pull/12", Checks: "✗ 1", HeadSHA: "deadbeef",
	})
	if started != 0 {
		t.Fatalf("auto-fix queued on imported PR (starts=%d)", started)
	}
	got, _ := store.Get("w_import_ci")
	got.NormalizePRs()
	if got.PRs[0].CIAutoFixCount != 0 || got.PRs[0].CINotifiedSHA != "" {
		t.Fatalf("CI fields written: %+v", got.PRs[0])
	}
}

func TestApplyPRInfoSkipsAutoLabelOnImported(t *testing.T) {
	b, store := testBotSessions(t)
	e := sessionstore.Entry{Project: "app", SessionKind: sessionstore.SessionKindImportedPR}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 7, State: "OPEN"})
	if err := store.Set("w_import_label", e); err != nil {
		t.Fatal(err)
	}
	if err := b.applyPRInfo(nil, "w_import_label", ghpr.Info{
		Number: 7, URL: "https://github.com/acme/app/pull/7", Title: "t",
		State: "OPEN", Owner: "acme", Repo: "app",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("w_import_label")
	if got.EffectiveLabel() != sessionstore.LabelOpen {
		t.Fatalf("label=%q want open", got.EffectiveLabel())
	}
}

func TestClearImportedPRKind(t *testing.T) {
	b, store := testBotSessions(t)
	e := sessionstore.Entry{Project: "app", SessionKind: sessionstore.SessionKindImportedPR}
	if err := store.Set("w_import_clear", e); err != nil {
		t.Fatal(err)
	}
	b.clearImportedPRKind("w_import_clear")
	got, _ := store.Get("w_import_clear")
	if got.IsImportedPR() || got.SessionKind != "" {
		t.Fatalf("kind=%q", got.SessionKind)
	}
}

func TestImportListFailureDoesNotDropSessions(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	withProjectGitHub(t, b.cfg, "app", "acme", "app")
	e := sessionstore.Entry{Project: "app"}
	e.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 1, State: "OPEN"})
	if err := store.Set("keep", e); err != nil {
		t.Fatal(err)
	}
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	if n := b.importOpenGitHubPRs(); n != 0 {
		t.Fatalf("imported=%d", n)
	}
	if _, ok := store.Get("keep"); !ok {
		t.Fatal("existing session dropped")
	}
}
