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

func TestDropShadowImportedPRsFromMultiRepoSession(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)

	api := sessionstore.TrackedPR{
		Owner: "acme", Repo: "api", Number: 4, State: "OPEN", HeadRef: "feat/api",
		URL: "https://github.com/acme/api/pull/4", Title: "api change",
	}
	web := sessionstore.TrackedPR{
		Owner: "acme", Repo: "web", Number: 8, State: "OPEN", HeadRef: "feat/web",
		URL: "https://github.com/acme/web/pull/8", Title: "web change",
	}
	creator := sessionstore.Entry{Project: "app", Goal: "ship both repos"}
	creator.UpsertPR(api)
	creator.UpsertPR(web)
	if err := store.Set("thread-creator", creator); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, project string
		pr          sessionstore.TrackedPR
	}{
		{"w_shadow_api", "app", api},
		{"w_shadow_web", "app", web},
		{"w_other_project", "other", api},
	} {
		shell := sessionstore.Entry{
			Project: tc.project, SessionKind: sessionstore.SessionKindImportedPR,
			Goal: tc.pr.Title, OwnerName: "zoe",
		}
		shell.UpsertPR(tc.pr)
		if err := store.Set(tc.id, shell); err != nil {
			t.Fatal(err)
		}
	}
	human := sessionstore.Entry{
		Project: "app", SessionKind: sessionstore.SessionKindImportedPR, Goal: "Human opened this",
	}
	human.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "api", Number: 99, State: "OPEN", HeadRef: "feat/human",
		URL: "https://github.com/acme/api/pull/99",
	})
	if err := store.Set("w_human", human); err != nil {
		t.Fatal(err)
	}
	// A shell that has already run is a normal work unit (the kind is cleared
	// at claim). It stays, next to the session that opened the PR.
	started := sessionstore.Entry{
		Project: "app", SessionID: "sess-started", Goal: "address already started",
	}
	started.UpsertPR(web)
	if err := store.Set("w_started", started); err != nil {
		t.Fatal(err)
	}
	review := sessionstore.Entry{
		Project: "app", SessionKind: sessionstore.SessionKindPRReview, Goal: "Review acme/web#8",
	}
	review.UpsertPR(web)
	if err := store.Set("thread-review", review); err != nil {
		t.Fatal(err)
	}

	// The page hides the untouched shell as soon as the creator is bound,
	// before the poller deletes it.
	hits := b.FindPRSessions("app", "acme", "web", 8)
	if len(hits) != 3 {
		t.Fatalf("FindPRSessions before drop: %+v", hits)
	}
	for _, h := range hits {
		if h.ThreadID == "w_shadow_web" {
			t.Fatal("untouched shadow must not be listed on the PR")
		}
	}
	if got := b.FindByPR("app", "acme", "web", 8, false); len(got) != 2 {
		t.Fatalf("Address hits = %+v, want creator and the started shell", got)
	}

	if n := b.dropShadowImportedPRs(); n != 2 {
		t.Fatalf("dropped=%d want 2", n)
	}
	for _, id := range []string{"w_shadow_api", "w_shadow_web"} {
		if _, ok := store.Get(id); ok {
			t.Fatalf("%s still stored", id)
		}
	}
	for _, id := range []string{"thread-creator", "w_other_project", "w_human", "w_started", "thread-review"} {
		if _, ok := store.Get(id); !ok {
			t.Fatalf("%s was deleted", id)
		}
	}
	hits = b.FindPRSessions("app", "acme", "api", 4)
	if len(hits) != 1 || hits[0].ThreadID != "thread-creator" {
		t.Fatalf("api PR sessions: %+v", hits)
	}
	if other := b.FindPRSessions("other", "acme", "api", 4); len(other) != 1 || other[0].ThreadID != "w_other_project" {
		t.Fatalf("other project: %+v", other)
	}
	if humanHits := b.FindPRSessions("app", "acme", "api", 99); len(humanHits) != 1 || !storeMust(t, store, "w_human").IsImportedPR() {
		t.Fatalf("human PR: %+v", humanHits)
	}

	pc := b.cfg.Projects["app"]
	pc.GitHub = &config.ProjectGitHubConfig{Repos: []config.GitHubRepoRef{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
	}}
	b.cfg.Projects["app"] = pc
	b.SetGHRunner(func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "--repo acme/api"):
			return []byte(`[{"number":4,"url":"https://github.com/acme/api/pull/4","title":"api change","state":"OPEN","author":{"login":"zoe"},"headRefName":"feat/api"}]`), nil
		case strings.Contains(joined, "--repo acme/web"):
			return []byte(`[{"number":8,"url":"https://github.com/acme/web/pull/8","title":"web change","state":"OPEN","author":{"login":"zoe"},"headRefName":"feat/web"}]`), nil
		default:
			t.Fatalf("unexpected gh: %v", args)
			return nil, nil
		}
	})
	if n := b.importOpenGitHubPRs(); n != 0 {
		t.Fatalf("reimported=%d", n)
	}
	if store.Count() != 5 {
		t.Fatalf("count=%d want creator+other+human+started+review", store.Count())
	}
}

func TestDropShadowKeepsSameNumberInAnotherRepo(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	admin := sessionstore.TrackedPR{
		Owner: "deploys-app", Repo: "admin", Number: 25, State: "OPEN",
		URL: "https://github.com/deploys-app/admin/pull/25",
	}
	collector := sessionstore.TrackedPR{
		Owner: "deploys-app", Repo: "collector", Number: 25, State: "OPEN",
		URL: "https://github.com/deploys-app/collector/pull/25",
	}
	work := sessionstore.Entry{Project: "deploys", Goal: "admin change"}
	work.UpsertPR(admin)
	if err := store.Set("thread-admin", work); err != nil {
		t.Fatal(err)
	}
	shell := sessionstore.Entry{
		Project: "deploys", SessionKind: sessionstore.SessionKindImportedPR, Goal: "collector change",
	}
	shell.UpsertPR(collector)
	if err := store.Set("w_collector", shell); err != nil {
		t.Fatal(err)
	}
	if n := b.dropShadowImportedPRs(); n != 0 {
		t.Fatalf("dropped=%d", n)
	}
	hits := b.FindPRSessions("deploys", "deploys-app", "collector", 25)
	if len(hits) != 1 || hits[0].ThreadID != "w_collector" {
		t.Fatalf("%+v", hits)
	}
	adminHits := b.FindPRSessions("deploys", "deploys-app", "admin", 25)
	if len(adminHits) != 1 || adminHits[0].ThreadID != "thread-admin" {
		t.Fatalf("admin: %+v", adminHits)
	}
}

func TestApplyPRInfoDropsShadowImport(t *testing.T) {
	b, store := testBotSessions(t)
	drainBotOnCleanup(t, b)
	if err := store.Set("thread-creator", sessionstore.Entry{Project: "app", Goal: "ship both repos"}); err != nil {
		t.Fatal(err)
	}
	shell := sessionstore.Entry{
		Project: "app", SessionKind: sessionstore.SessionKindImportedPR, Goal: "api change", OwnerName: "zoe",
	}
	shell.UpsertPR(sessionstore.TrackedPR{
		Owner: "acme", Repo: "api", Number: 4, State: "OPEN",
		URL: "https://github.com/acme/api/pull/4", HeadRef: "feat/api",
	})
	if err := store.Set("w_shadow_api", shell); err != nil {
		t.Fatal(err)
	}
	if err := b.applyPRInfo(nil, "thread-creator", ghpr.Info{
		Number: 4, URL: "https://github.com/acme/api/pull/4", Title: "api change",
		State: "OPEN", Owner: "acme", Repo: "api", HeadRef: "feat/api",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("w_shadow_api"); ok {
		t.Fatal("shadow survived the creator binding the PR")
	}
	hits := b.FindPRSessions("app", "acme", "api", 4)
	if len(hits) != 1 || hits[0].ThreadID != "thread-creator" {
		t.Fatalf("%+v", hits)
	}
}

func storeMust(t *testing.T, store *sessionstore.Store, id string) sessionstore.Entry {
	t.Helper()
	e, ok := store.Get(id)
	if !ok {
		t.Fatalf("missing %s", id)
	}
	return e
}
