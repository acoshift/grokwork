package bot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// The owner filter is what makes the board usable for two different audiences:
// an engineer looking for unclaimed escalations, and anyone looking for their own
// cases. "Mine" spans engineer and thread ownership so neither has to know which
// field their role lands in.
func TestListCaseBoardOwnerFilter(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"alpha": filepath.Join(dir, "alpha")}),
		DataDir:  dir,
	}
	if err := os.MkdirAll(filepath.Join(dir, "alpha"), 0o755); err != nil {
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
	seed := map[string]sessionstore.Entry{
		// Escalated and claimed by the viewer.
		"c-mine-eng": {
			Project: "alpha", Mode: "case", Phase: "fixing",
			CustomerTitle: "Mine as engineer", EngineerID: "u-eng", EngineerName: "eng",
		},
		// Escalated by support: no engineer, but the viewer filed it.
		"c-mine-owner": {
			Project: "alpha", Mode: "case", Phase: "fixing",
			CustomerTitle: "Mine as reporter", OwnerID: "u-eng",
		},
		// Someone else's, and claimed.
		"c-theirs": {
			Project: "alpha", Mode: "case", Phase: "fixing",
			CustomerTitle: "Theirs", EngineerID: "u-other", EngineerName: "other",
			OwnerID: "u-support",
		},
		// Nobody's: the triage queue.
		"c-open": {
			Project: "alpha", Mode: "case", Phase: "fixing", CustomerTitle: "Unclaimed",
		},
		// Co-owner counts as mine.
		"c-co": {
			Project: "alpha", Mode: "case", Phase: "investigate",
			CustomerTitle: "Co-owned", OwnerID: "u-support", CoOwnerIDs: []string{"u-eng"},
			EngineerID: "u-other", EngineerName: "other",
		},
	}
	for id, e := range seed {
		if err := store.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	b := New(cfg, store, hist)

	ids := func(board CaseBoard) []string {
		var out []string
		for _, g := range board.Groups {
			for _, r := range g.Rows {
				out = append(out, r.ThreadID)
			}
		}
		slices.Sort(out)
		return out
	}

	mine := b.ListCaseBoardQuery(CaseBoardQuery{
		Project: "alpha", Owner: CaseOwnerMine, ViewerID: "u-eng",
	})
	if got, want := ids(mine), []string{"c-co", "c-mine-eng", "c-mine-owner"}; !slices.Equal(got, want) {
		t.Fatalf("mine=%v want %v", got, want)
	}

	unassigned := b.ListCaseBoardQuery(CaseBoardQuery{
		Project: "alpha", Owner: CaseOwnerUnassigned, ViewerID: "u-eng",
	})
	if got, want := ids(unassigned), []string{"c-mine-owner", "c-open"}; !slices.Equal(got, want) {
		t.Fatalf("unassigned=%v want %v", got, want)
	}

	// Counts are over the pre-filter set so the labels do not change as you filter.
	if mine.Mine != 3 || mine.Unassigned != 2 || unassigned.Mine != 3 || unassigned.Unassigned != 2 {
		t.Fatalf("counts mine=%+v unassigned=%+v", mine, unassigned)
	}

	// No viewer (unauthenticated / no identity) must not turn "mine" into
	// "everything with no engineer".
	anon := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", Owner: CaseOwnerMine})
	if anon.Shown != 0 {
		t.Fatalf("anonymous mine shown=%d want 0", anon.Shown)
	}

	// An unrecognized value falls back to no filter rather than hiding everything.
	junk := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", Owner: "sql-injection", ViewerID: "u-eng"})
	if junk.OwnerFilter != "" || junk.Shown != 5 {
		t.Fatalf("junk owner filter=%q shown=%d", junk.OwnerFilter, junk.Shown)
	}
}

// The breach filter is a lead's "what is late" view, and its dropdown label has
// to equal the rows picking it returns — the same property the owner counters
// have, now that two filters can narrow the same board.
func TestListCaseBoardSLAFilter(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	minutes := func(n int) *int { return &n }
	cfg := &config.Config{
		Projects: config.ProjectsMap{"alpha": {
			Path: proj,
			// One hour to first response, four to resolve; low has no SLA at all.
			SLA: map[string]config.SLATarget{
				"critical": {FirstResponseMinutes: minutes(60), ResolutionMinutes: minutes(240)},
				"high":     {FirstResponseMinutes: minutes(60)},
			},
		}},
		DataDir: dir,
		// The last case clears the SLA table through the real setter, which
		// persists.
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-5 * time.Hour).Format(time.RFC3339)
	recent := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	seed := map[string]sessionstore.Entry{
		// Late, unclaimed, and the viewer's: counts for every filter.
		"c-late-mine": {
			Project: "alpha", Mode: "case", Phase: "fixing", Severity: "critical",
			CustomerTitle: "Late and mine", OwnerID: "u-me", OpenedAt: old,
		},
		// Late but someone else's.
		"c-late-theirs": {
			Project: "alpha", Mode: "case", Phase: "fixing", Severity: "high",
			CustomerTitle: "Late, theirs", OwnerID: "u-other",
			EngineerID: "u-other", EngineerName: "other", OpenedAt: old,
		},
		// Filed five minutes ago: inside both targets.
		"c-fresh": {
			Project: "alpha", Mode: "case", Phase: "fixing", Severity: "critical",
			CustomerTitle: "Fresh", OwnerID: "u-me", OpenedAt: recent,
		},
		// Old, but its severity has no target — never late.
		"c-no-target": {
			Project: "alpha", Mode: "case", Phase: "fixing", Severity: "low",
			CustomerTitle: "No SLA", OwnerID: "u-me", OpenedAt: old,
		},
		// Old and critical, but answered in time and now waiting on the customer:
		// the resolution clock is frozen, so it is not late either.
		"c-held": {
			Project: "alpha", Mode: "case", Phase: "answered", Severity: "critical",
			CustomerTitle: "On hold", OwnerID: "u-me", OpenedAt: old,
			FirstResponseAt: time.Now().UTC().Add(-4*time.Hour - 30*time.Minute).Format(time.RFC3339),
			AnsweredAt:      time.Now().UTC().Add(-4*time.Hour - 30*time.Minute).Format(time.RFC3339),
		},
	}
	for id, e := range seed {
		if err := store.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	b := New(cfg, store, hist)

	ids := func(board CaseBoard) []string {
		var out []string
		for _, g := range board.Groups {
			for _, r := range g.Rows {
				out = append(out, r.ThreadID)
			}
		}
		slices.Sort(out)
		return out
	}

	all := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", ViewerID: "u-me"})
	if got, want := ids(all), []string{"c-fresh", "c-held", "c-late-mine", "c-late-theirs", "c-no-target"}; !slices.Equal(got, want) {
		t.Fatalf("unfiltered=%v want %v", got, want)
	}
	if all.Breached != 2 {
		t.Fatalf("breached count = %d want 2 (%+v)", all.Breached, ids(all))
	}

	breached := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", SLA: CaseSLABreached, ViewerID: "u-me"})
	if got, want := ids(breached), []string{"c-late-mine", "c-late-theirs"}; !slices.Equal(got, want) {
		t.Fatalf("breached=%v want %v", got, want)
	}
	// The label equals the rows: this is the invariant, checked against the rows
	// themselves rather than a hardcoded number.
	if breached.Breached != breached.Shown || breached.Shown != len(ids(breached)) {
		t.Fatalf("count %d != shown %d", breached.Breached, breached.Shown)
	}
	// Every row that survived the filter carries a breach chip.
	for _, g := range breached.Groups {
		for _, r := range g.Rows {
			if !r.SLABreached || r.SLABadge == "" {
				t.Fatalf("row %s missing its badge: %+v", r.ThreadID, r.SLA)
			}
		}
	}

	// Cross-filter counters: Mine is counted after the SLA filter (2 of the 3
	// mine-cases are not late), and Breached after the owner filter.
	both := b.ListCaseBoardQuery(CaseBoardQuery{
		Project: "alpha", SLA: CaseSLABreached, Owner: CaseOwnerMine, ViewerID: "u-me",
	})
	if got, want := ids(both), []string{"c-late-mine"}; !slices.Equal(got, want) {
		t.Fatalf("mine+breached=%v want %v", got, want)
	}
	if both.Mine != 1 {
		t.Fatalf("Mine=%d want 1 — the label must count only late cases while the SLA filter is on", both.Mine)
	}
	if both.Breached != 1 {
		t.Fatalf("Breached=%d want 1 — the label must respect the owner filter", both.Breached)
	}
	// Phase counters stay project-wide regardless of either filter.
	if both.Total != 5 || both.Fixing != 4 {
		t.Fatalf("pipeline counts moved: %+v", both)
	}

	// An unrecognized value falls back to no filter rather than hiding the board.
	junk := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", SLA: "; drop table", ViewerID: "u-me"})
	if junk.SLAFilter != "" || junk.Shown != 5 {
		t.Fatalf("junk sla filter=%q shown=%d", junk.SLAFilter, junk.Shown)
	}

	// A project with no SLA table badges nothing, so the filter selects nothing.
	if err := cfg.SetProjectSLA("alpha", nil); err != nil {
		t.Fatal(err)
	}
	none := b.ListCaseBoardQuery(CaseBoardQuery{Project: "alpha", SLA: CaseSLABreached, ViewerID: "u-me"})
	if none.Shown != 0 || none.Breached != 0 {
		t.Fatalf("no targets configured: shown=%d breached=%d", none.Shown, none.Breached)
	}
}

// Escalating is a handoff: an engineer doing it takes the case, support doing it
// leaves it for engineering to pick up. The outcome is returned rather than
// re-derived from caps, because the two disagree when there is no actor id.
func TestEscalateCaseOwnershipByRole(t *testing.T) {
	newBot := func(t *testing.T) (*Bot, *sessionstore.Store) {
		t.Helper()
		dir := t.TempDir()
		store, err := sessionstore.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Projects: config.PathProjects(map[string]string{"app": filepath.Join(dir, "app")})}
		return New(cfg, store, nil), store
	}
	seed := func(t *testing.T, store *sessionstore.Store, tid, phase, engID string) {
		t.Helper()
		e := sessionstore.Entry{
			Project: "app", Mode: ModeCase, Phase: phase,
			CustomerTitle: "Checkout fails", OwnerID: "u-support",
		}
		if engID != "" {
			e.EngineerID, e.EngineerName = engID, "Prior Eng"
		}
		if err := store.Set(tid, e); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("builder claims an unassigned case", func(t *testing.T) {
		b, store := newBot(t)
		seed(t, store, "c1", sessionstore.PhaseIntake, "")
		out, err := b.EscalateCase(EscalateCaseOpts{
			ThreadID: "c1", Actor: Actor{ID: "u-eng", DisplayName: "Eng"}, TakeOwnership: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Assigned || out.EngineerID != "u-eng" || out.Released {
			t.Fatalf("outcome=%+v", out)
		}
		e, _ := store.Get("c1")
		if e.EngineerID != "u-eng" || e.EngineerName == "" {
			t.Fatalf("must claim: %+v", e)
		}
		// Thread ownership gates cancel/reset and is not the engineering assignment.
		if e.OwnerID != "u-support" {
			t.Fatalf("thread owner changed: %q", e.OwnerID)
		}
	})

	t.Run("support handoff leaves it unassigned", func(t *testing.T) {
		b, store := newBot(t)
		seed(t, store, "c2", sessionstore.PhaseInvestigate, "")
		out, err := b.EscalateCase(EscalateCaseOpts{
			ThreadID: "c2", Actor: Actor{ID: "u-support", DisplayName: "Sup"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Assigned || out.EngineerID != "" {
			t.Fatalf("outcome=%+v", out)
		}
		e, _ := store.Get("c2")
		if e.EngineerID != "" || e.EscalatedBy != "u-support" || e.Phase != sessionstore.PhaseFixing {
			t.Fatalf("entry=%+v", e)
		}
		if e.OwnerID != "u-support" {
			t.Fatalf("thread owner must survive: %q", e.OwnerID)
		}
	})

	// A support re-escalate on a case already with engineering is a nudge. Clearing
	// the engineer here would erase a name the escalator cannot even see.
	t.Run("support re-escalate keeps the current engineer", func(t *testing.T) {
		b, store := newBot(t)
		seed(t, store, "c3", sessionstore.PhaseFixing, "u-eng")
		out, err := b.EscalateCase(EscalateCaseOpts{
			ThreadID: "c3", Actor: Actor{ID: "u-support", DisplayName: "Sup"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Released || out.EngineerID != "u-eng" {
			t.Fatalf("outcome=%+v", out)
		}
		if e, _ := store.Get("c3"); e.EngineerID != "u-eng" {
			t.Fatalf("engineer yanked by a nudge: %+v", e)
		}
	})

	// Web auth off: builder-class caps resolve true but there is no id to record.
	// Claiming is impossible, so the assignment must be left alone — and the caller
	// must not be told "assigned to you".
	t.Run("no actor id neither claims nor clears", func(t *testing.T) {
		b, store := newBot(t)
		seed(t, store, "c4", sessionstore.PhaseIntake, "u-eng")
		out, err := b.EscalateCase(EscalateCaseOpts{ThreadID: "c4", TakeOwnership: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Assigned || out.Released || out.EngineerID != "u-eng" {
			t.Fatalf("outcome=%+v", out)
		}
		if e, _ := store.Get("c4"); e.EngineerID != "u-eng" {
			t.Fatalf("assignment lost with no actor id: %+v", e)
		}
	})
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
