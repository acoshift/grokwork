package bot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestShipRowFromCase(t *testing.T) {
	e := sessionstore.Entry{
		Project: "p", Mode: "case", Phase: sessionstore.PhaseShipping,
		CustomerTitle: "Checkout broken",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/1", Number: 1, State: "OPEN",
			Owner: "acme", Repo: "app",
		}},
	}
	row := shipRowFrom("tid", e, e.PRs[0], "goal", false, 0)
	if !row.FromCase || row.CasePhase != sessionstore.PhaseShipping || row.CaseTitle != "Checkout broken" {
		t.Fatalf("%+v", row)
	}
	e.Mode = "fix"
	row = shipRowFrom("tid", e, e.PRs[0], "goal", false, 0)
	if row.FromCase {
		t.Fatal("non-case should not be FromCase")
	}
}

func TestListShipBoard(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{
			"alpha": filepath.Join(dir, "alpha"),
			"beta":  filepath.Join(dir, "beta"),
		}),
		DataDir: dir,
	}
	for _, pc := range cfg.Projects {
		if err := os.MkdirAll(pc.Path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("t1", history.Turn{
		User: "alice", Prompt: "fix the flaky payment timeout",
		Response: "done", Status: "done", Project: "alpha",
	}); err != nil {
		t.Fatal(err)
	}

	// Open PR with failing checks.
	if err := store.Set("t1", sessionstore.Entry{
		SessionID: "s1",
		Project:   "alpha",
		OwnerID:   "u1",
		OwnerName: "alice",
		Goal:      "fix payment timeout",
		Label:     sessionstore.LabelNeedsReview,
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/acme/alpha/pull/10",
			Number: 10,
			State:  "OPEN",
			Title:  "fix payment timeout",
			Checks: "✓ 2 · ✗ 1",
			Review: "REVIEW_REQUIRED",
			Owner:  "acme",
			Repo:   "alpha",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Draft on beta.
	if err := store.Set("t2", sessionstore.Entry{
		SessionID: "s2",
		Project:   "beta",
		OwnerName: "bob",
		PRs: []sessionstore.TrackedPR{{
			URL:     "https://github.com/acme/beta/pull/3",
			Number:  3,
			State:   "OPEN",
			IsDraft: true,
			Title:   "wip feature",
			Checks:  "… 1",
			Owner:   "acme",
			Repo:    "beta",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Merged (terminal).
	if err := store.Set("t3", sessionstore.Entry{
		SessionID: "s3",
		Project:   "alpha",
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/acme/alpha/pull/9",
			Number: 9,
			State:  "MERGED",
			Title:  "already shipped",
			Owner:  "acme",
			Repo:   "alpha",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Session without PRs — ignored.
	if err := store.Set("t4", sessionstore.Entry{
		SessionID: "s4",
		Project:   "alpha",
	}); err != nil {
		t.Fatal(err)
	}

	b := New(cfg, store, hist)

	open := b.ListShipBoard("", "open")
	if open.Open != 2 || open.Draft != 1 || open.ChecksFailing != 1 || open.Merged != 1 || open.Total != 3 {
		t.Fatalf("open stats: open=%d draft=%d fail=%d merged=%d total=%d",
			open.Open, open.Draft, open.ChecksFailing, open.Merged, open.Total)
	}
	if len(open.Rows) != 2 {
		t.Fatalf("open rows: %d want 2", len(open.Rows))
	}
	// Failing PR first.
	if open.Rows[0].Number != 10 || !open.Rows[0].ChecksFailing {
		t.Fatalf("first row should be failing #10: %+v", open.Rows[0])
	}
	if open.Rows[0].State != "OPEN" || open.Rows[1].State != "DRAFT" {
		t.Fatalf("states: %s %s", open.Rows[0].State, open.Rows[1].State)
	}

	all := b.ListShipBoard("", "all")
	if len(all.Rows) != 3 {
		t.Fatalf("all rows: %d", len(all.Rows))
	}

	alpha := b.ListShipBoard("alpha", "all")
	if len(alpha.Rows) != 2 || alpha.Total != 2 {
		t.Fatalf("alpha all: rows=%d total=%d", len(alpha.Rows), alpha.Total)
	}
	failing := b.ListShipBoard("", "failing")
	if len(failing.Rows) != 1 || failing.Rows[0].Number != 10 {
		t.Fatalf("failing: %+v", failing.Rows)
	}
	merged := b.ListShipBoard("alpha", "merged")
	if len(merged.Rows) != 1 || merged.Rows[0].Number != 9 {
		t.Fatalf("merged: %+v", merged.Rows)
	}
	draft := b.ListShipBoard("beta", "draft")
	if len(draft.Rows) != 1 || draft.Rows[0].Number != 3 {
		t.Fatalf("draft: %+v", draft.Rows)
	}

	// ACL-style among filter: only alpha rows + stats, dropdown is the among list.
	onlyAlpha := b.ListShipBoardAmong("", "all", []string{"alpha"})
	if len(onlyAlpha.Rows) != 2 || onlyAlpha.Total != 2 {
		t.Fatalf("among alpha: rows=%d total=%d", len(onlyAlpha.Rows), onlyAlpha.Total)
	}
	if len(onlyAlpha.Projects) != 1 || onlyAlpha.Projects[0] != "alpha" {
		t.Fatalf("among projects: %v", onlyAlpha.Projects)
	}
	hidden := b.ListShipBoardAmong("beta", "all", []string{"alpha"})
	if len(hidden.Rows) != 0 || hidden.Total != 0 {
		t.Fatalf("among denied project filter: %+v", hidden)
	}
	empty := b.ListShipBoardAmong("", "open", []string{})
	if len(empty.Rows) != 0 || len(empty.Projects) != 0 {
		t.Fatalf("among empty: rows=%d projects=%v", len(empty.Rows), empty.Projects)
	}
}

// TestShipBoardMergesUnitsOnOnePR pins the one-row-per-PR merge. Two units bind
// #7: an idle one holding the current poll (the poller skips busy threads) and a
// running one whose copy froze when its run started. The row must take PR facts
// from the idle unit, still say "running", and open the running unit.
func TestShipBoardMergesUnitsOnOnePR(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"alpha": filepath.Join(dir, "alpha")}),
		DataDir:  dir,
	}
	if err := os.MkdirAll(cfg.Projects["alpha"].Path, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	pr := sessionstore.TrackedPR{
		URL: "https://github.com/acme/alpha/pull/7", Number: 7,
		Owner: "acme", Repo: "alpha", State: "OPEN", Title: "fix retry queue",
	}
	// Idle unit: freshest poll — CI has since gone red.
	stale := pr
	stale.Checks = "✓ 3"
	fresh := pr
	fresh.Checks = "✓ 2 · ✗ 1"
	if err := store.Set("t-idle", sessionstore.Entry{
		SessionID: "s-idle", Project: "alpha", OwnerName: "idle-owner",
		Goal: "review pass", Label: sessionstore.LabelDone,
		PRs: []sessionstore.TrackedPR{fresh},
	}); err != nil {
		t.Fatal(err)
	}
	// Busy unit: its PR copy froze at run start, so it is the stale one.
	if err := store.Set("t-busy", sessionstore.Entry{
		SessionID: "s-busy", Project: "alpha", OwnerName: "busy-owner",
		Goal: "address CI", Label: sessionstore.LabelInProgress,
		PRs: []sessionstore.TrackedPR{stale},
	}); err != nil {
		t.Fatal(err)
	}

	b := New(cfg, store, hist)
	// A run in flight is what makes t-busy's PR copy the stale one.
	b.states.Store("t-busy", &threadState{job: &runJob{}})

	board := b.ListShipBoard("alpha", "all")
	if len(board.Rows) != 1 {
		t.Fatalf("want 1 merged row, got %d: %+v", len(board.Rows), board.Rows)
	}
	if board.Total != 1 {
		t.Fatalf("stats must count PRs, not pairs: total=%d", board.Total)
	}
	row := board.Rows[0]
	if row.SessionCount != 2 {
		t.Fatalf("SessionCount=%d want 2", row.SessionCount)
	}
	// PR facts from the idle unit — the busy one's "✓ 3" is stale.
	if row.Checks != "✓ 2 · ✗ 1" || !row.ChecksFailing {
		t.Fatalf("PR facts not from the freshest copy: checks=%q failing=%v", row.Checks, row.ChecksFailing)
	}
	// Session facts from the running unit, and the run still shows.
	if row.ThreadID != "t-busy" || row.OwnerName != "busy-owner" || row.Goal != "address CI" {
		t.Fatalf("session facts not from the live unit: %+v", row)
	}
	if !row.Running {
		t.Fatal("merged row must still report the in-flight run")
	}
	// Least-terminal label wins: one unit being done does not make the PR done.
	if row.Label != sessionstore.LabelInProgress {
		t.Fatalf("Label=%q want %q", row.Label, sessionstore.LabelInProgress)
	}

	// A second, unrelated PR stays its own row.
	if err := store.Set("t-other", sessionstore.Entry{
		SessionID: "s-other", Project: "alpha",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/alpha/pull/8", Number: 8,
			Owner: "acme", Repo: "alpha", State: "OPEN",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if board := b.ListShipBoard("alpha", "all"); len(board.Rows) != 2 {
		t.Fatalf("distinct PRs must not merge: %d rows", len(board.Rows))
	}
}

// TestMergeShipRows pins the per-field merge rules. It works on rows directly
// because sessionstore stamps UpdatedAt on every write, so the recency tiebreaks
// are unreachable through the store.
func TestMergeShipRows(t *testing.T) {
	base := func(thread, checks, updated string) ShipPRRow {
		return ShipPRRow{
			ThreadID: thread, Project: "alpha",
			URL: "https://github.com/acme/alpha/pull/7", Number: 7,
			GHOwner: "acme", GHRepo: "alpha", State: "OPEN",
			Checks: checks, ChecksFailing: checksLookFailing(checks), UpdatedAt: updated,
		}
	}

	t.Run("freshest PR facts, liveliest session", func(t *testing.T) {
		busy := base("t-busy", "✓ 3", "2026-07-25T13:00:00Z")
		busy.Running, busy.OwnerName, busy.Goal = true, "busy-owner", "address CI"
		busy.Label = sessionstore.LabelInProgress
		idle := base("t-idle", "✓ 2 · ✗ 1", "2026-07-25T12:00:00Z")
		idle.OwnerName, idle.Goal, idle.Label = "idle-owner", "review pass", sessionstore.LabelDone
		idle.Queue = 2

		got := mergeShipRows([]ShipPRRow{busy, idle})
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d", len(got))
		}
		row := got[0]
		// The busy unit is NEWER but its PR copy froze at run start — the poller
		// skips busy threads — so the idle unit's red CI is the truth.
		if row.Checks != "✓ 2 · ✗ 1" || !row.ChecksFailing {
			t.Fatalf("PR facts: checks=%q failing=%v", row.Checks, row.ChecksFailing)
		}
		if row.ThreadID != "t-busy" || row.OwnerName != "busy-owner" || row.Goal != "address CI" {
			t.Fatalf("session facts: %+v", row)
		}
		if !row.Running || row.Queue != 2 {
			t.Fatalf("aggregates: running=%v queue=%d", row.Running, row.Queue)
		}
		if row.Label != sessionstore.LabelInProgress {
			t.Fatalf("one unit being done must not mark the PR done: %q", row.Label)
		}
		if row.UpdatedAt != "2026-07-25T13:00:00Z" {
			t.Fatalf("UpdatedAt=%q want the latest movement", row.UpdatedAt)
		}
		if row.SessionCount != 2 {
			t.Fatalf("SessionCount=%d", row.SessionCount)
		}
	})

	t.Run("idle pair takes the newer PR copy", func(t *testing.T) {
		old := base("t-old", "✓ 1", "2026-07-25T10:00:00Z")
		recent := base("t-zz", "✗ 1", "2026-07-25T11:00:00Z")
		got := mergeShipRows([]ShipPRRow{old, recent})
		if len(got) != 1 || got[0].Checks != "✗ 1" {
			t.Fatalf("PR facts must follow the newer poll: %+v", got)
		}
		// ...but the unit to open is chosen without UpdatedAt, or the Owner and
		// Goal cells would swap on every poll tick. See livelierSession.
		if got[0].ThreadID != "t-old" {
			t.Fatalf("session pick must be recency-independent, got %q", got[0].ThreadID)
		}
	})

	t.Run("session pick holds still as timestamps move", func(t *testing.T) {
		a := base("t-a", "✓ 1", "2026-07-25T10:00:00Z")
		b := base("t-b", "✓ 1", "2026-07-25T11:00:00Z")
		first := mergeShipRows([]ShipPRRow{a, b})[0].ThreadID
		// Next poll cycle flips which one was written last.
		a.UpdatedAt, b.UpdatedAt = "2026-07-25T12:00:00Z", "2026-07-25T11:30:00Z"
		if second := mergeShipRows([]ShipPRRow{a, b})[0].ThreadID; second != first {
			t.Fatalf("merged row changed unit on a poll tick: %q → %q", first, second)
		}
	})

	t.Run("one unit listing a PR twice is one session", func(t *testing.T) {
		dup := base("t-dup", "✓ 1", "2026-07-25T10:00:00Z")
		got := mergeShipRows([]ShipPRRow{dup, dup})
		if len(got) != 1 || got[0].SessionCount != 1 {
			t.Fatalf("SessionCount counts units, not rows: %+v", got)
		}
	})

	t.Run("owner/repo and URL forms of the same PR merge", func(t *testing.T) {
		byURL := base("t-url", "✓ 1", "2026-07-25T10:00:00Z")
		byRef := ShipPRRow{
			ThreadID: "t-ref", Project: "alpha", Number: 7,
			GHOwner: "acme", GHRepo: "alpha", State: "OPEN",
			UpdatedAt: "2026-07-25T09:00:00Z",
		}
		if got := mergeShipRows([]ShipPRRow{byURL, byRef}); len(got) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(got), got)
		}
	})

	t.Run("distinct PRs and projects stay apart", func(t *testing.T) {
		a := base("t-a", "", "2026-07-25T10:00:00Z")
		other := base("t-b", "", "2026-07-25T10:00:00Z")
		other.Number, other.URL = 8, "https://github.com/acme/alpha/pull/8"
		crossProject := base("t-c", "", "2026-07-25T10:00:00Z")
		crossProject.Project = "beta"
		if got := mergeShipRows([]ShipPRRow{a, other, crossProject}); len(got) != 3 {
			t.Fatalf("want 3 rows, got %d: %+v", len(got), got)
		}
	})

	t.Run("unidentifiable PRs never merge", func(t *testing.T) {
		// No URL, no owner/repo, no number: nothing says these are the same PR.
		a := ShipPRRow{ThreadID: "t-a", Project: "alpha", State: "OPEN"}
		b := ShipPRRow{ThreadID: "t-b", Project: "alpha", State: "OPEN"}
		if got := mergeShipRows([]ShipPRRow{a, b}); len(got) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
		}
	})

	t.Run("single row still reports its count", func(t *testing.T) {
		got := mergeShipRows([]ShipPRRow{base("t-a", "✓ 1", "2026-07-25T10:00:00Z")})
		if len(got) != 1 || got[0].SessionCount != 1 {
			t.Fatalf("%+v", got)
		}
	})
}

func TestChecksLookFailing(t *testing.T) {
	if !checksLookFailing("✓ 2 · ✗ 1") {
		t.Fatal("expected failing")
	}
	if checksLookFailing("✓ 3") {
		t.Fatal("expected pass")
	}
	if checksLookFailing("") {
		t.Fatal("empty not failing")
	}
}

func TestSortShipRowsStableAcrossUpdatedAt(t *testing.T) {
	// Same attention rank, different session activity — order must not follow UpdatedAt.
	rows := []ShipPRRow{
		{ThreadID: "t-new", Project: "alpha", GHOwner: "acme", GHRepo: "app", Number: 10, State: "OPEN", RawState: "OPEN", UpdatedAt: "2026-07-21T12:00:00Z"},
		{ThreadID: "t-old", Project: "alpha", GHOwner: "acme", GHRepo: "app", Number: 20, State: "OPEN", RawState: "OPEN", UpdatedAt: "2026-07-01T00:00:00Z"},
		{ThreadID: "t-fail", Project: "beta", GHOwner: "acme", GHRepo: "api", Number: 3, State: "OPEN", RawState: "OPEN", ChecksFailing: true, UpdatedAt: "2026-07-10T00:00:00Z"},
		{ThreadID: "t-merge", Project: "alpha", GHOwner: "acme", GHRepo: "app", Number: 5, State: "MERGED", RawState: "MERGED", UpdatedAt: "2026-07-20T00:00:00Z"},
		{ThreadID: "t-draft", Project: "alpha", GHOwner: "acme", GHRepo: "app", Number: 15, State: "DRAFT", RawState: "OPEN", UpdatedAt: "2026-07-19T00:00:00Z"},
	}
	// Shuffle input order; result must be deterministic.
	for i := range 3 {
		in := append([]ShipPRRow(nil), rows...)
		if i == 1 {
			in[0], in[3] = in[3], in[0]
		}
		if i == 2 {
			in[1], in[2] = in[2], in[1]
		}
		sortShipRows(in)
		// Fail first, then open by project/repo/#desc, then draft, then merged.
		want := []string{"t-fail", "t-old", "t-new", "t-draft", "t-merge"}
		for j, id := range want {
			if in[j].ThreadID != id {
				t.Fatalf("pass %d pos %d: got %q want %q (rows=%+v)", i, j, in[j].ThreadID, id, in)
			}
		}
	}
}
