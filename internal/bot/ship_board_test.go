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
	for i := 0; i < 3; i++ {
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
