package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

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
	if !strings.Contains(open.Digest, "acme/alpha#10") || !strings.Contains(open.Digest, "CI failing: 1") {
		t.Fatalf("digest missing content:\n%s", open.Digest)
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
	// Digest stays open-PR focused even when the table filter is terminal-only.
	if !strings.Contains(merged.Digest, "acme/alpha#10") || strings.Contains(merged.Digest, "already shipped") {
		t.Fatalf("merged filter digest should list open PRs only:\n%s", merged.Digest)
	}
	draft := b.ListShipBoard("beta", "draft")
	if len(draft.Rows) != 1 || draft.Rows[0].Number != 3 {
		t.Fatalf("draft: %+v", draft.Rows)
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
