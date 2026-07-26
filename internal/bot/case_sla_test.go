package bot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// A first-response target is breached once it passes and not before. The clock
// itself is the only source: nothing stores "breached".
func TestCaseSLAFirstResponseBreach(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	target := time.Hour

	late := sessionstore.Entry{
		Mode: ModeCase, Phase: sessionstore.PhaseIntake, Severity: "critical",
		OpenedAt: stamp(now.Add(-90 * time.Minute)),
	}
	got := computeCaseSLA(late, target, 0, now)
	if !got.Active || !got.Breached || !got.FirstResponse.Breached {
		t.Fatalf("90m against a 1h target must breach: %+v", got)
	}
	if got.FirstResponse.Over() != 30*time.Minute {
		t.Fatalf("over = %v want 30m", got.FirstResponse.Over())
	}
	if got.Badge == "" || got.Detail == "" {
		t.Fatalf("breach needs a chip: %+v", got)
	}

	inside := late
	inside.OpenedAt = stamp(now.Add(-40 * time.Minute))
	got = computeCaseSLA(inside, target, 0, now)
	if !got.Active || got.Breached {
		t.Fatalf("40m against a 1h target must not breach: %+v", got)
	}
	if got.FirstResponse.Remaining() != 20*time.Minute {
		t.Fatalf("remaining = %v want 20m", got.FirstResponse.Remaining())
	}

	// Exactly on target is met, not missed.
	onTarget := late
	onTarget.OpenedAt = stamp(now.Add(-time.Hour))
	if computeCaseSLA(onTarget, target, 0, now).Breached {
		t.Fatal("elapsed == target must not breach")
	}

	// Answering in time stops the clock for good: it stays met however long the
	// case then stays open.
	answered := late
	answered.FirstResponseAt = stamp(now.Add(-80 * time.Minute))
	got = computeCaseSLA(answered, target, 0, now)
	if got.Breached || !got.FirstResponse.Stopped {
		t.Fatalf("responded in 10m: %+v", got)
	}

	// Responding late stays breached after the fact — a met SLA is not something
	// a slow reply can retroactively earn.
	lateReply := late
	lateReply.FirstResponseAt = stamp(now.Add(-10 * time.Minute))
	if got = computeCaseSLA(lateReply, target, 0, now); !got.Breached {
		t.Fatalf("late reply must stay breached: %+v", got)
	}
}

// No target means no SLA. This is the case that a naive zero-value int would get
// wrong: a project that never configured deadlines must not have every case
// screaming.
func TestCaseSLAUnsetTargetNeverBreaches(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ancient := sessionstore.Entry{
		Mode: ModeCase, Phase: sessionstore.PhaseFixing, Severity: "critical",
		OpenedAt: stamp(now.AddDate(-1, 0, 0)),
	}
	got := computeCaseSLA(ancient, 0, 0, now)
	if got.Active || got.Breached || got.Badge != "" {
		t.Fatalf("a year old with no target: %+v", got)
	}
	// One clock configured, the other not: only the configured one exists.
	got = computeCaseSLA(ancient, 0, 30*24*time.Hour, now)
	if got.FirstResponse.Active {
		t.Fatal("first-response clock must stay inactive when its target is unset")
	}
	if !got.Resolution.Active || !got.Breached {
		t.Fatalf("resolution clock: %+v", got.Resolution)
	}

	// A case filed before SLA timestamps existed has no start to measure from,
	// and UpdatedAt is not a substitute — guessing would breach the whole
	// archive on the first deploy.
	legacy := ancient
	legacy.OpenedAt = ""
	if got = computeCaseSLA(legacy, time.Hour, time.Hour, now); got.Active || got.Breached {
		t.Fatalf("pre-SLA case: %+v", got)
	}

	// Not a case at all: no clocks, whatever the project configures.
	engFix := sessionstore.Entry{Mode: "fix", Severity: "critical", OpenedAt: stamp(now.Add(-time.Hour))}
	if computeCaseSLA(engFix, time.Minute, time.Minute, now).Active {
		t.Fatal("eng sessions have no SLA")
	}
}

// PAUSE SEMANTICS: the resolution clock freezes while the case waits on the
// customer (phase answered), and unfreezes if the case comes back to us. See
// caseSLAHold for why the pause is not accumulated.
func TestCaseSLAPausesWhileWaitingOnCustomer(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-10 * time.Hour)
	answered := now.Add(-9 * time.Hour) // handed back one hour in
	target := 4 * time.Hour

	held := sessionstore.Entry{
		Mode: ModeCase, Phase: sessionstore.PhaseAnswered, Severity: "high",
		OpenedAt: stamp(opened), FirstResponseAt: stamp(answered), AnsweredAt: stamp(answered),
	}
	got := computeCaseSLA(held, time.Hour, target, now)
	if !got.Held || got.Breached {
		t.Fatalf("waiting on the customer must not burn the clock: %+v", got.Resolution)
	}
	if got.Resolution.Elapsed != time.Hour {
		t.Fatalf("clock must freeze at the handoff: elapsed=%v want 1h", got.Resolution.Elapsed)
	}
	if got.Badge != "SLA · on hold" {
		t.Fatalf("badge = %q", got.Badge)
	}

	// A case that had already blown its target before we answered stays breached:
	// pausing forgives the wait after the handoff, not the one before it.
	lateHandoff := held
	lateHandoff.AnsweredAt = stamp(opened.Add(5 * time.Hour))
	if got = computeCaseSLA(lateHandoff, time.Hour, target, now); !got.Breached {
		t.Fatalf("breach before the handoff must survive it: %+v", got.Resolution)
	}

	// The customer replies and the case comes back to us: the clock runs again
	// from the original start (the documented, deliberately conservative
	// direction — it can over-report, never under-report).
	back := held
	back.Phase = sessionstore.PhaseFixing
	if got = computeCaseSLA(back, time.Hour, target, now); got.Held || !got.Breached {
		t.Fatalf("resumed clock: %+v", got.Resolution)
	}

	// Answered before AnsweredAt existed: keep counting rather than freezing at
	// an unknown instant.
	unstamped := held
	unstamped.AnsweredAt = ""
	if got = computeCaseSLA(unstamped, time.Hour, target, now); got.Held || !got.Breached {
		t.Fatalf("unstamped hold: %+v", got.Resolution)
	}
}

func TestCaseSLAStopsOnClose(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-30 * time.Hour)

	// Resolved inside target, then left closed for a day: still met.
	closed := sessionstore.Entry{
		Mode: ModeCase, Phase: sessionstore.PhaseClosed, Severity: "medium",
		OpenedAt: stamp(opened), FirstResponseAt: stamp(opened.Add(10 * time.Minute)),
		ResolvedAt: stamp(opened.Add(2 * time.Hour)), Resolution: "fixed",
	}
	got := computeCaseSLA(closed, time.Hour, 4*time.Hour, now)
	if got.Breached || !got.Resolution.Stopped {
		t.Fatalf("closed in time: %+v", got)
	}
	if got.Badge != "SLA · met" {
		t.Fatalf("badge = %q", got.Badge)
	}

	// Closed as a duplicate without ever replying: the first-response clock stops
	// at the close instead of growing forever, but the miss is still recorded.
	silent := closed
	silent.FirstResponseAt = ""
	silent.Resolution = "duplicate"
	got = computeCaseSLA(silent, time.Hour, 4*time.Hour, now)
	if !got.FirstResponse.Stopped || !got.FirstResponse.Breached {
		t.Fatalf("never replied: %+v", got.FirstResponse)
	}
	if got.FirstResponse.Elapsed != 2*time.Hour {
		t.Fatalf("first-response clock should stop at the close: %v", got.FirstResponse.Elapsed)
	}
}

// Hand-edited or skewed stamps must not produce a negative clock that reads as
// comfortably inside target.
func TestCaseSLAClampsImpossibleOrder(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	e := sessionstore.Entry{
		Mode: ModeCase, Phase: sessionstore.PhaseClosed, Severity: "low",
		OpenedAt: stamp(now), ResolvedAt: stamp(now.Add(-5 * time.Hour)),
	}
	got := computeCaseSLA(e, time.Hour, time.Hour, now)
	if got.Resolution.Elapsed != 0 || got.Breached {
		t.Fatalf("negative elapsed: %+v", got.Resolution)
	}
}

// The lifecycle actions both surfaces share have to stamp the clocks, or the SLA
// measures nothing. Escalating deliberately does not count as a response — it
// tells engineering, not the customer.
func TestCaseSLAStampsAcrossLifecycle(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Projects: config.PathProjects(map[string]string{"app": proj}), DataDir: dir}
	b := New(cfg, store, nil)

	if err := b.ensureCaseShell("c1", "app", Actor{ID: "u-support", DisplayName: "Sup"},
		"high", "ZD-1", "Checkout fails", SourceWeb); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get("c1")
	if _, ok := e.SLARoundStart(); !ok {
		t.Fatalf("intake must start the clock: %+v", e)
	}
	opened := e.OpenedAt

	// Escalation is internal.
	if _, err := b.EscalateCase(EscalateCaseOpts{ThreadID: "c1", Actor: Actor{ID: "u-eng"}, TakeOwnership: true}); err != nil {
		t.Fatal(err)
	}
	if e, _ = store.Get("c1"); e.FirstResponseAt != "" {
		t.Fatalf("escalating is not a customer response: %q", e.FirstResponseAt)
	}

	// A customer update is.
	if _, _, err := b.SetCaseCustomerUpdate("c1", "We found the cause and are fixing it."); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get("c1")
	if e.FirstResponseAt == "" {
		t.Fatal("customer update must stamp the first response")
	}
	firstResponse := e.FirstResponseAt

	// Answering holds the resolution clock and keeps the original first response.
	if err := b.AnswerCase("c1", "u-support", "Here is the workaround."); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get("c1")
	if e.AnsweredAt == "" || e.FirstResponseAt != firstResponse {
		t.Fatalf("answer stamps: answered=%q first=%q", e.AnsweredAt, e.FirstResponseAt)
	}

	// Closing records the resolution; reopening starts a fresh round and clears
	// the response, because the customer is waiting again.
	if err := b.CloseCase("c1", "u-support", "answered", "done"); err != nil {
		t.Fatal(err)
	}
	if e, _ = store.Get("c1"); e.ResolvedAt == "" {
		t.Fatal("close must stamp ResolvedAt")
	}
	if err := b.ReopenCase("c1", "u-support", sessionstore.PhaseInvestigate); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get("c1")
	if e.FirstResponseAt != "" || e.AnsweredAt != "" || e.ResolvedAt != "" {
		t.Fatalf("reopen must clear the round: %+v", e)
	}
	if e.OpenedAt != opened {
		t.Fatalf("reopen must keep the filing time: %q want %q", e.OpenedAt, opened)
	}
	start, ok := e.SLARoundStart()
	if !ok || start.Format(time.RFC3339) != e.ReopenedAt {
		t.Fatalf("new round must start at the reopen: %v vs %q", start, e.ReopenedAt)
	}
}

// The severity vocabulary an SLA can be keyed by has to cover every severity
// intake can produce, or that severity is one nobody can write a deadline for.
// config cannot import bot (or sessionstore), so the two lists are pinned here.
func TestSeverityVocabularyMatchesIntake(t *testing.T) {
	for _, in := range []string{
		"low", "medium", "high", "critical",
		"sev1", "sev2", "sev3", "sev4",
		"", "nonsense", "CRITICAL",
	} {
		got := normalizeSeverity(in)
		if !slices.Contains(config.SLASeverities, got) {
			t.Fatalf("normalizeSeverity(%q) = %q, which config.SLASeverities cannot express", in, got)
		}
	}
	if len(config.SLASeverities) != 4 {
		t.Fatalf("SLASeverities = %v; a new severity needs an intake alias too", config.SLASeverities)
	}
}
