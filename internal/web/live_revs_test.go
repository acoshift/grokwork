package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestLiveRevsStableAndChange(t *testing.T) {
	srv, cfg, _ := testServer(t)

	a := srv.computeLiveRevs()
	b := srv.computeLiveRevs()
	if a != b {
		t.Fatalf("revs unstable without changes:\n a=%+v\n b=%+v", a, b)
	}
	for _, rev := range []string{a.Dashboard, a.Ship, a.Cases, a.History, a.Worktrees, a.Config} {
		if rev == "" {
			t.Fatal("expected non-empty revs")
		}
	}

	// Config mutation should move Config fingerprint.
	names := cfg.ProjectNames()
	if len(names) == 0 {
		t.Fatal("no projects")
	}
	if err := cfg.AddProjectAllowedUser(names[0], "user-live-rev"); err != nil {
		t.Fatal(err)
	}
	c := srv.computeLiveRevs()
	if c.Config == a.Config {
		t.Fatal("config rev should change after allowlist add")
	}
	if c.Ship != a.Ship {
		t.Fatal("ship rev should not change on allowlist add")
	}

	// History append moves history rev.
	beforeHist := c.History
	if err := srv.history.Append("thread-new", history.Turn{
		User: "bob", Prompt: "do thing", Response: "done", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}
	d := srv.computeLiveRevs()
	if d.History == beforeHist {
		t.Fatal("history rev should change after new turn")
	}

	// Session with PR moves ship rev.
	beforeShip := d.Ship
	if err := srv.sessions.Set("thread-pr", sessionstore.Entry{
		SessionID: "s1",
		Project:   "proj",
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/o/r/pull/1",
			Number: 1,
			State:  "OPEN",
			Title:  "feat",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	e := srv.computeLiveRevs()
	if e.Ship == beforeShip {
		t.Fatal("ship rev should change after PR session")
	}

	// Case session mutation moves the cases rev (but a phase change alone
	// must not move the ship rev — separate domains).
	beforeCases := e.Cases
	if err := srv.sessions.Set("thread-case", sessionstore.Entry{
		SessionID: "s2",
		Project:   "proj",
		Mode:      "case",
		Phase:     sessionstore.PhaseIntake,
		Severity:  "high",
	}); err != nil {
		t.Fatal(err)
	}
	f := srv.computeLiveRevs()
	if f.Cases == beforeCases {
		t.Fatal("cases rev should change after case session")
	}
	beforeShip = f.Ship
	if _, _, err := srv.sessions.Patch("thread-case", func(e *sessionstore.Entry) {
		e.Phase = sessionstore.PhaseInvestigate
	}); err != nil {
		t.Fatal(err)
	}
	g := srv.computeLiveRevs()
	if g.Cases == f.Cases {
		t.Fatal("cases rev should change after phase transition")
	}
	if g.Ship != beforeShip {
		t.Fatal("ship rev should not change on a case phase transition")
	}
}

func TestComputeLiveRevsHistoryMovesOnRunIdle(t *testing.T) {
	srv, _, _ := testServer(t)
	idle := srv.computeLiveRevs()
	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj", "prompt", "live"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })
	running := srv.computeLiveRevs()
	if running.History == idle.History {
		t.Fatal("cached history rev must move when a run starts")
	}
	if running.Dashboard == idle.Dashboard {
		t.Fatal("dashboard rev should move when a run starts")
	}
	bot.FinishRunForTest(srv.bot, "thread-99")
	after := srv.computeLiveRevs()
	if after.History == running.History {
		t.Fatal("cached history rev must move when a run ends")
	}
}

func TestComputeLiveRevsIdleSkipsHistoryReread(t *testing.T) {
	srv, cfg, _ := testServer(t)
	before := srv.computeLiveRevs()
	path := filepath.Join(cfg.DataDir, "history", "thread-99.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := srv.computeLiveRevs()
	if after.History != before.History {
		t.Fatal("idle tick must not re-parse history files")
	}
	if after.Ship != before.Ship || after.Cases != before.Cases || after.Worktrees != before.Worktrees {
		t.Fatalf("idle tick rebuilt expensive domains:\n before=%+v\n after=%+v", before, after)
	}
}

func TestNextSLARecompute(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	empty := nextSLARecompute(bot.CaseBoard{}, now)
	if empty.Sub(now) != 24*time.Hour {
		t.Fatalf("empty board until=%s", empty)
	}

	board := bot.CaseBoard{
		Groups: []bot.CaseGroup{{
			Rows: []bot.CaseRow{{
				SLA: bot.CaseSLA{
					FirstResponse: bot.SLAClock{Active: true, Target: time.Hour, Elapsed: 50 * time.Minute},
					Resolution:    bot.SLAClock{Active: true, Target: 4 * time.Hour, Elapsed: time.Hour, Held: true},
				},
			}},
		}},
	}
	got := nextSLARecompute(board, now)
	if got.Sub(now) != 10*time.Minute {
		t.Fatalf("soonest remaining=%s want 10m", got.Sub(now))
	}
}

// TestHistoryRevMovesOnRunIdleAndSessionChrome pins the session-detail live
// region: #live-session listens on history, so a turn finishing (and the
// Work unit chrome patched after history.Append) must move that rev — without
// ticking on in-flight stream text the way dashboard does.
func TestHistoryRevMovesOnRunIdleAndSessionChrome(t *testing.T) {
	srv, _, _ := testServer(t)
	idle := srv.fpHistory()

	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj", "prompt", "live so far"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })

	running := srv.fpHistory()
	if running == idle {
		t.Fatal("history rev should change when a run starts")
	}
	again := srv.fpHistory()
	if again != running {
		t.Fatal("history rev must stay put while a run is in flight (elapsed/live text are dashboard)")
	}

	bot.FinishRunForTest(srv.bot, "thread-99")
	after := srv.fpHistory()
	if after == running {
		t.Fatal("history rev should change when a run ends")
	}

	beforePR := after
	if _, _, err := srv.sessions.Patch("thread-99", func(e *sessionstore.Entry) {
		e.PRs = []sessionstore.TrackedPR{{
			Number: 7, State: "OPEN", Title: "feat", Owner: "acme", Repo: "app",
		}}
	}); err != nil {
		t.Fatal(err)
	}
	if srv.fpHistory() == beforePR {
		t.Fatal("history rev should change when Work unit PR chrome is patched")
	}

	beforeVerify := srv.fpHistory()
	if _, _, err := srv.sessions.Patch("thread-99", func(e *sessionstore.Entry) {
		e.LastVerify = &sessionstore.LastVerify{Name: "unit", OK: true, At: "2026-08-28T00:00:00Z"}
	}); err != nil {
		t.Fatal(err)
	}
	if srv.fpHistory() == beforeVerify {
		t.Fatal("history rev should change when last-verify chrome is patched")
	}

	if _, _, err := srv.sessions.Patch("thread-99", func(e *sessionstore.Entry) {
		e.Mode = "case"
		e.Phase = sessionstore.PhaseClosed
		e.Resolution = "fixed"
	}); err != nil {
		t.Fatal(err)
	}
	beforeNote := srv.fpHistory()
	if _, _, err := srv.sessions.Patch("thread-99", func(e *sessionstore.Entry) {
		e.ResolutionNote = "shipped in 4.2.3"
	}); err != nil {
		t.Fatal(err)
	}
	if srv.fpHistory() == beforeNote {
		t.Fatal("history rev should change when the close note is patched")
	}
}
