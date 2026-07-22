package bot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestListCaseBoard(t *testing.T) {
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

	seed := map[string]sessionstore.Entry{
		// Unknown phase buckets as intake; Goal fallback title.
		"c-intake": {
			SessionID: "s1", Project: "alpha", Mode: "case", Phase: "bogus",
			Severity: "critical", Goal: "EU checkout broken",
			ReporterName: "beam",
		},
		"c-investigate": {
			SessionID: "s2", Project: "alpha", Mode: "case", Phase: "investigate",
			Severity: "high", CustomerTitle: "Webhook retries duplicated",
			CustomerRef: "ZD-4821", OwnerName: "mint",
			CustomerUpdate: "We are reproducing the duplicate retries now.",
		},
		"c-answered": {
			SessionID: "s3", Project: "alpha", Mode: "case", Phase: "answered",
			Severity: "low", CustomerTitle: "How do refunds settle?",
		},
		"c-shipping": {
			SessionID: "s4", Project: "alpha", Mode: "case", Phase: "shipping",
			Severity: "high", CustomerTitle: "Rate limit header missing",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/alpha/pull/12", Number: 12,
				State: "OPEN", Checks: "✓ 2 · ✗ 1", Owner: "acme", Repo: "alpha",
			}},
		},
		"c-closed": {
			SessionID: "s5", Project: "alpha", Mode: "case", Phase: "closed",
			CustomerTitle: "Old ticket", Resolution: "fixed",
		},
		// Other project and non-case sessions must not leak in.
		"c-beta": {
			SessionID: "s6", Project: "beta", Mode: "case", Phase: "intake",
			CustomerTitle: "Beta-only case",
		},
		"eng-fix": {
			SessionID: "s7", Project: "alpha", Mode: "fix", Goal: "not a case",
		},
	}
	for id, e := range seed {
		if err := store.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}

	b := New(cfg, store, hist)

	open := b.ListCaseBoard("alpha", "", "", "")
	if open.Intake != 1 || open.Investigate != 1 || open.Answered != 1 || open.Fixing != 0 ||
		open.Shipping != 1 || open.Closed != 1 || open.OpenTotal != 4 || open.Total != 5 {
		t.Fatalf("counts: %+v", open)
	}
	// Closed hidden by default; groups follow pipeline order.
	if open.Shown != 4 || len(open.Groups) != 4 {
		t.Fatalf("open shown=%d groups=%d", open.Shown, len(open.Groups))
	}
	wantOrder := []string{"intake", "investigate", "answered", "shipping"}
	for i, g := range open.Groups {
		if g.Phase != wantOrder[i] {
			t.Fatalf("group %d = %q want %q", i, g.Phase, wantOrder[i])
		}
		if g.Plain == "" {
			t.Fatalf("group %q missing plain-language label", g.Phase)
		}
	}
	// Unknown phase bucketed as intake with Goal fallback title.
	if got := open.Groups[0].Rows[0]; got.ThreadID != "c-intake" || got.Title != "EU checkout broken" {
		t.Fatalf("intake row: %+v", got)
	}
	// PR chip data on the shipping case.
	ship := open.Groups[3].Rows[0]
	if ship.PRNumber != 12 || ship.PRState != "OPEN" || !ship.PRChecksFailing ||
		ship.GHOwner != "acme" || ship.GHRepo != "alpha" {
		t.Fatalf("shipping PR row: %+v", ship)
	}

	all := b.ListCaseBoard("alpha", "", "", "all")
	if all.Shown != 5 || len(all.Groups) != 5 {
		t.Fatalf("all shown=%d groups=%d", all.Shown, len(all.Groups))
	}
	closedGroup := all.Groups[len(all.Groups)-1]
	if closedGroup.Phase != "closed" || closedGroup.Rows[0].Resolution != "fixed" {
		t.Fatalf("closed group: %+v", closedGroup)
	}

	// Phase filter closed shows closed even in default scope.
	closed := b.ListCaseBoard("alpha", "closed", "", "")
	if closed.Shown != 1 || closed.Groups[0].Rows[0].ThreadID != "c-closed" {
		t.Fatalf("closed filter: %+v", closed)
	}

	// Severity filter narrows rows but keeps pipeline counts project-wide.
	high := b.ListCaseBoard("alpha", "", "high", "")
	if high.Shown != 2 || high.Total != 5 {
		t.Fatalf("severity filter: shown=%d total=%d", high.Shown, high.Total)
	}

	// Empty project = all projects (SSE fingerprint path).
	every := b.ListCaseBoard("", "", "", "all")
	if every.Shown != 6 {
		t.Fatalf("all projects shown=%d", every.Shown)
	}
}

func TestCaseSeverityTriageSort(t *testing.T) {
	rows := []CaseRow{
		{ThreadID: "t-low", Severity: "low", UpdatedAt: "2026-07-21T12:00:00Z"},
		{ThreadID: "t-none", UpdatedAt: "2026-07-22T12:00:00Z"},
		{ThreadID: "t-crit-old", Severity: "critical", UpdatedAt: "2026-07-01T00:00:00Z"},
		{ThreadID: "t-crit-new", Severity: "critical", UpdatedAt: "2026-07-20T00:00:00Z"},
		{ThreadID: "t-high", Severity: "high", UpdatedAt: "2026-07-10T00:00:00Z"},
	}
	sortCaseRows(rows)
	want := []string{"t-crit-new", "t-crit-old", "t-high", "t-low", "t-none"}
	for i, id := range want {
		if rows[i].ThreadID != id {
			t.Fatalf("pos %d: got %q want %q", i, rows[i].ThreadID, id)
		}
	}
}
