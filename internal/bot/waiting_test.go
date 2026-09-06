package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func minutes(n int) *int { return &n }

func waitingBot(t *testing.T) (*Bot, *sessionstore.Store, *history.Store) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "alpha")
	secret := filepath.Join(dir, "secret")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{
			"alpha": {
				Path: proj,
				SLA: map[string]config.SLATarget{
					"critical": {FirstResponseMinutes: minutes(60)},
				},
			},
			"secret": {
				Path: secret,
				SLA: map[string]config.SLATarget{
					"critical": {FirstResponseMinutes: minutes(60)},
				},
			},
		},
		DataDir: dir,
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, hist), store, hist
}

func mustSet(t *testing.T, store *sessionstore.Store, id string, e sessionstore.Entry) {
	t.Helper()
	if err := store.Set(id, e); err != nil {
		t.Fatal(err)
	}
}

func rowIDs(board WaitingBoard) []string {
	out := make([]string, 0, len(board.Rows))
	for _, r := range board.Rows {
		out = append(out, r.ThreadID)
	}
	return out
}

func TestListWaitingOnYouEmptyActor(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "t1", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "x",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "ship?"}},
	})
	got := b.ListWaitingOnYou(WaitingQuery{})
	if got.Matched != 0 || len(got.Rows) != 0 {
		t.Fatalf("empty actor must not dump the instance: %+v", got)
	}
}

func TestListWaitingOnYouAmongEmptyAndUnknownProject(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "t1", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "x",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "ship?"}},
	})
	empty := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1", Among: []string{}})
	if empty.Matched != 0 {
		t.Fatalf("empty Among = %d want 0", empty.Matched)
	}
	miss := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1", Project: "secret", Among: []string{"alpha"}})
	if miss.Matched != 0 {
		t.Fatalf("project not in Among = %d want 0", miss.Matched)
	}
}

func TestListWaitingOnYouHiddenProjectDoesNotOccupyCap(t *testing.T) {
	b, store, _ := waitingBot(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for i := range 40 {
		mustSet(t, store, fmt.Sprintf("secret-%02d", i), sessionstore.Entry{
			Project: "secret", Mode: ModeCase, Phase: sessionstore.PhaseIntake,
			Severity: "critical", OwnerID: "u1", CustomerTitle: "hidden SLA",
			OpenedAt: old, UpdatedAt: old,
		})
	}
	mustSet(t, store, "visible", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "visible decision",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "ok?"}},
		UpdatedAt:     now.UTC().Format(time.RFC3339),
	})
	got := b.ListWaitingOnYouAt(WaitingQuery{ActorID: "u1", Among: []string{"alpha"}}, now)
	if got.Matched != 1 || got.Shown != 1 {
		t.Fatalf("matched=%d shown=%d rows=%v", got.Matched, got.Shown, rowIDs(got))
	}
	if got.Rows[0].ThreadID != "visible" {
		t.Fatalf("hidden SLA occupied the cap: %+v", got.Rows[0])
	}
	if slices.ContainsFunc(got.Rows, func(r WaitingRow) bool {
		return r.Project == "secret" || strings.Contains(r.Title, "hidden")
	}) {
		t.Fatalf("secret leaked: %+v", got.Rows)
	}
}

func TestListWaitingOnYouInvolvementAndNoGenericCase(t *testing.T) {
	b, store, _ := waitingBot(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mustSet(t, store, "owner-sla", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseIntake,
		Severity: "critical", OwnerID: "u1", CustomerTitle: "owner sla",
		OpenedAt: old,
	})
	mustSet(t, store, "eng-sla", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseFixing,
		Severity: "critical", EngineerID: "u1", CustomerTitle: "eng sla",
		OpenedAt: old,
	})
	mustSet(t, store, "co-ci", sessionstore.Entry{
		Project: "alpha", OwnerID: "other", CoOwnerIDs: []string{"u1"}, Goal: "co ci",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/alpha/pull/1", Number: 1,
			State: "OPEN", Checks: "✓ 1 · ✗ 1", Owner: "acme", Repo: "alpha",
		}},
	})
	mustSet(t, store, "watch-q", sessionstore.Entry{
		Project: "alpha", OwnerID: "other", WatcherIDs: []string{"u1"}, Goal: "watch q",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "proceed?"}},
	})
	mustSet(t, store, "healthy-case", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseIntake,
		Severity: "critical", OwnerID: "u1", CustomerTitle: "just filed",
		OpenedAt: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	})
	mustSet(t, store, "stranger", sessionstore.Entry{
		Project: "alpha", OwnerID: "nope", Goal: "not yours",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "no"}},
	})

	got := b.ListWaitingOnYouAt(WaitingQuery{ActorID: "u1"}, now)
	ids := rowIDs(got)
	slices.Sort(ids)
	if want := []string{"co-ci", "eng-sla", "owner-sla", "watch-q"}; !slices.Equal(ids, want) {
		t.Fatalf("rows=%v want %v", ids, want)
	}
	if slices.Contains(ids, "healthy-case") {
		t.Fatal("owned open case with no SLA/decision/CI must not appear")
	}
	if slices.Contains(ids, "stranger") {
		t.Fatal("stranger row leaked")
	}
}

func TestCaseIsMineExcludesWatcherOnly(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "watched", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseFixing,
		CustomerTitle: "Watched", OwnerID: "u-owner", WatcherIDs: []string{"u1"},
	})
	board := b.ListCaseBoardQuery(CaseBoardQuery{
		Project: "alpha", Owner: CaseOwnerMine, ViewerID: "u1",
	})
	if board.Shown != 0 {
		t.Fatalf("watcher-only must not be case-board mine: shown=%d", board.Shown)
	}
	if caseIsMine(sessionstore.Entry{WatcherIDs: []string{"u1"}, OwnerID: "u-owner"}, "u1") {
		t.Fatal("caseIsMine grew a watcher clause")
	}
}

func TestListWaitingOnYouSkipsAskTerminalClosed(t *testing.T) {
	b, store, _ := waitingBot(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mustSet(t, store, "ask", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", SessionKind: sessionstore.SessionKindPRAsk,
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "ask?"}},
	})
	mustSet(t, store, "done", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Label: sessionstore.LabelDone, Goal: "shipped",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "leftover"}},
	})
	mustSet(t, store, "closed-case", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseClosed,
		Severity: "critical", OwnerID: "u1", CustomerTitle: "closed",
		OpenedAt: old,
	})
	got := b.ListWaitingOnYouAt(WaitingQuery{ActorID: "u1"}, now)
	if got.Matched != 0 {
		t.Fatalf("skipped units leaked: %v", rowIDs(got))
	}
}

func TestListWaitingOnYouOpenQuestionStatus(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "answered", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "answered",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "done", Status: "answered", Answer: "yes"}},
	})
	mustSet(t, store, "dismissed", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "dismissed",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "nah", Status: "dismissed"}},
	})
	mustSet(t, store, "empty", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "empty status",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "still open"}},
	})
	mustSet(t, store, "open", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "explicit open",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "open", Status: "open"}},
	})
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	ids := rowIDs(got)
	slices.Sort(ids)
	if want := []string{"empty", "open"}; !slices.Equal(ids, want) {
		t.Fatalf("rows=%v want %v", ids, want)
	}
}

func TestListWaitingOnYouOneRowTwoReasons(t *testing.T) {
	b, store, _ := waitingBot(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mustSet(t, store, "both", sessionstore.Entry{
		Project: "alpha", Mode: ModeCase, Phase: sessionstore.PhaseFixing,
		Severity: "critical", OwnerID: "u1", CustomerTitle: "late and blocked",
		OpenedAt:      old,
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "bump timeout?"}},
	})
	got := b.ListWaitingOnYouAt(WaitingQuery{ActorID: "u1"}, now)
	if got.Matched != 1 {
		t.Fatalf("matched=%d want 1 (%v)", got.Matched, rowIDs(got))
	}
	r := got.Rows[0]
	if r.Kind != waitingKindCase {
		t.Fatalf("kind=%q", r.Kind)
	}
	if !slices.Equal(r.Reasons, []WaitingReason{WaitingReasonSLA, WaitingReasonDecision}) {
		t.Fatalf("reasons=%v", r.Reasons)
	}
	if r.SortRank != 0 {
		t.Fatalf("sortRank=%d want 0 (sla)", r.SortRank)
	}
	if r.DecisionText != "bump timeout?" {
		t.Fatalf("decision=%q", r.DecisionText)
	}
}

func TestListWaitingOnYouCoOwnerCINotMergePrimary(t *testing.T) {
	b, store, _ := waitingBot(t)
	pr := sessionstore.TrackedPR{
		URL: "https://github.com/acme/alpha/pull/9", Number: 9,
		State: "OPEN", Checks: "✗ 2", Owner: "acme", Repo: "alpha",
	}
	mustSet(t, store, "primary", sessionstore.Entry{
		Project: "alpha", OwnerID: "other", Goal: "primary owner",
		PRs: []sessionstore.TrackedPR{pr},
	})
	mustSet(t, store, "co", sessionstore.Entry{
		Project: "alpha", OwnerID: "other", CoOwnerIDs: []string{"u1"}, Goal: "co-owned",
		PRs: []sessionstore.TrackedPR{pr},
	})
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	if got.Matched != 1 || got.Rows[0].ThreadID != "co" {
		t.Fatalf("co-owner CI hidden by merge primary: %v", rowIDs(got))
	}
	if got.Rows[0].PRNumber != 9 || !slices.Contains(got.Rows[0].Reasons, WaitingReasonCI) {
		t.Fatalf("row=%+v", got.Rows[0])
	}
}

func TestListWaitingOnYouCapReportsMatched(t *testing.T) {
	b, store, _ := waitingBot(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for i := range 41 {
		ts := now.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
		mustSet(t, store, fmt.Sprintf("u-%02d", i), sessionstore.Entry{
			Project: "alpha", OwnerID: "u1", Goal: fmt.Sprintf("q %02d", i),
			OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "x"}},
			UpdatedAt:     ts,
		})
	}
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	if got.Matched != 41 || got.Shown != 40 || got.Cap != 40 {
		t.Fatalf("matched=%d shown=%d cap=%d", got.Matched, got.Shown, got.Cap)
	}
	if got.Rows[0].ThreadID != "u-40" {
		t.Fatalf("newest first after rank: first=%s", got.Rows[0].ThreadID)
	}
	if got.Rows[39].ThreadID != "u-01" {
		t.Fatalf("40th=%s want u-01 (oldest of the kept window)", got.Rows[39].ThreadID)
	}
}

func TestListWaitingOnYouTitleNeverLastPrompt(t *testing.T) {
	b, store, hist := waitingBot(t)
	const secret = "SECRET CUSTOMER PROMPT"
	if err := hist.Append("t-prompt", history.Turn{Prompt: secret, Project: "alpha"}); err != nil {
		t.Fatal(err)
	}
	mustSet(t, store, "t-prompt", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1",
		OpenQuestions: []sessionstore.OpenQuestion{{ID: "q", Text: "go?"}},
	})
	if preview := b.lastPromptPreview("t-prompt"); preview != secret {
		t.Fatalf("fixture: lastPromptPreview=%q", preview)
	}
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	if got.Matched != 1 {
		t.Fatalf("matched=%d", got.Matched)
	}
	if got.Rows[0].Title != waitingUntitled {
		t.Fatalf("title=%q want untitled", got.Rows[0].Title)
	}
	if strings.Contains(got.Rows[0].Title, secret) {
		t.Fatal("prompt leaked into Today title")
	}
}

func TestListWaitingOnYouTeamReview(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "sess", sessionstore.Entry{
		Project: "alpha", OwnerID: "other", Goal: "the pr",
	})
	if _, err := b.Reviews().RequestReview(reviewstore.Request{
		Owner: "acme", Repo: "alpha", Number: 4, Project: "alpha",
		ThreadID: "sess", RequesterID: "lead", ReviewerID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	if got.Matched != 1 || got.Rows[0].Kind != waitingKindReview {
		t.Fatalf("review row: %+v", got)
	}
	if got.Rows[0].PRNumber != 4 || got.Rows[0].Title != "the pr" {
		t.Fatalf("review title/pr: %+v", got.Rows[0])
	}
}

func TestListWaitingOnYouReviewTitleStaysOnRequestProject(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "secret-sess", sessionstore.Entry{
		Project: "secret", OwnerID: "u1", Goal: "secret-goal",
	})
	if _, err := b.Reviews().RequestReview(reviewstore.Request{
		Owner: "acme", Repo: "alpha", Number: 8, Project: "alpha",
		ThreadID: "secret-sess", RequesterID: "lead", ReviewerID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1", Among: []string{"alpha"}})
	if got.Matched != 1 {
		t.Fatalf("matched=%d rows=%v", got.Matched, rowIDs(got))
	}
	if strings.Contains(got.Rows[0].Title, "secret") {
		t.Fatalf("hidden session goal leaked onto review title: %q", got.Rows[0].Title)
	}
	if got.Rows[0].Title != "acme/alpha#8" {
		t.Fatalf("title=%q want owner/repo#n", got.Rows[0].Title)
	}
}

func TestListWaitingOnYouMergedPRNotCI(t *testing.T) {
	b, store, _ := waitingBot(t)
	mustSet(t, store, "merged", sessionstore.Entry{
		Project: "alpha", OwnerID: "u1", Goal: "done pr",
		PRs: []sessionstore.TrackedPR{{
			Number: 3, State: "MERGED", Checks: "✗ 1", Owner: "acme", Repo: "alpha",
		}},
	})
	got := b.ListWaitingOnYou(WaitingQuery{ActorID: "u1"})
	if got.Matched != 0 {
		t.Fatalf("terminal PR: %v", rowIDs(got))
	}
}
