package bot

import (
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestCaseActionsLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Projects: config.PathProjects(map[string]string{"app": filepath.Join(dir, "app")})}
	b := New(cfg, store, nil)
	tid := "t-case-1"
	if err := store.Set(tid, sessionstore.Entry{
		Project: "app", Mode: ModeCase, Phase: sessionstore.PhaseIntake,
		CustomerTitle: "Checkout fails", Severity: "high", OwnerID: "u1",
		Dossier: &sessionstore.Dossier{Summary: "timeout in payment gateway"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.EscalateCase(tid, "u-eng", "stack in thread"); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get(tid)
	if e.Phase != sessionstore.PhaseFixing || e.Mode != ModeCase || e.EscalatedBy != "u-eng" {
		t.Fatalf("after escalate: %+v", e)
	}
	// answer path from fixing without close
	if err := b.AnswerCase(tid, "u1", "Please update the app"); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get(tid)
	if e.Phase != sessionstore.PhaseAnswered || e.CustomerUpdate == "" {
		t.Fatalf("after answer: %+v", e)
	}
	if _, _, err := b.SetCaseCustomerUpdate(tid, "Safe reply for customer"); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseCase(tid, "u1", "answered", ""); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get(tid)
	if !e.IsCaseClosed() || e.Resolution != "answered" {
		t.Fatalf("after close: %+v", e)
	}
	if err := b.EscalateCase(tid, "u-eng", ""); err != ErrCaseClosed {
		t.Fatalf("want ErrCaseClosed, got %v", err)
	}
	if err := b.AnswerCase(tid, "u1", ""); err != ErrCaseClosed {
		t.Fatalf("answer on closed want ErrCaseClosed, got %v", err)
	}

	// Reopen default → investigate; clear resolution*; keep dossier.
	if err := b.ReopenCase(tid, "u-inv", ""); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get(tid)
	if e.Mode != ModeCase {
		t.Fatalf("mode after reopen: %q", e.Mode)
	}
	if e.Phase != sessionstore.PhaseInvestigate {
		t.Fatalf("phase after reopen: %q", e.Phase)
	}
	if e.IsCaseClosed() {
		t.Fatal("IsCaseClosed true after reopen")
	}
	if e.Resolution != "" || e.ResolutionNote != "" || e.ResolvedAt != "" || e.ResolvedBy != "" {
		t.Fatalf("resolution fields not cleared: %+v", e)
	}
	if e.Dossier == nil || e.Dossier.Summary != "timeout in payment gateway" {
		t.Fatalf("dossier wiped: %+v", e.Dossier)
	}
	if e.Label != sessionstore.LabelOpen {
		t.Fatalf("label after reopen investigate: %q", e.Label)
	}
	// Closed gates open again.
	if err := b.EscalateCase(tid, "u-eng", "still broken"); err != nil {
		t.Fatalf("escalate after reopen: %v", err)
	}
	e, _ = store.Get(tid)
	if e.Phase != sessionstore.PhaseFixing {
		t.Fatalf("phase after re-escalate: %q", e.Phase)
	}
}

func TestReopenCaseRequiresClosedAndValidPhase(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Projects: config.PathProjects(map[string]string{"app": filepath.Join(dir, "app")})}
	b := New(cfg, store, nil)

	if err := b.ReopenCase("missing", "u1", ""); err != ErrCaseNoSession {
		t.Fatalf("missing session: got %v", err)
	}
	if err := store.Set("t-fix", sessionstore.Entry{
		Project: "app", Mode: ModeFix, Phase: "",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReopenCase("t-fix", "u1", ""); err != ErrNotACase {
		t.Fatalf("non-case: got %v", err)
	}
	if err := store.Set("t-open", sessionstore.Entry{
		Project: "app", Mode: ModeCase, Phase: sessionstore.PhaseInvestigate,
		CustomerTitle: "open",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReopenCase("t-open", "u1", ""); err != ErrCaseNotClosed {
		t.Fatalf("already open: got %v", err)
	}
	// Open case phase must not be clobbered by a failed reopen.
	e, _ := store.Get("t-open")
	if e.Phase != sessionstore.PhaseInvestigate {
		t.Fatalf("open phase clobbered: %q", e.Phase)
	}

	tid := "t-reopen-fixing"
	if err := store.Set(tid, sessionstore.Entry{
		Project: "app", Mode: ModeCase, Phase: sessionstore.PhaseClosed,
		CustomerTitle: "Still broken", Resolution: "fixed", ResolutionNote: "shipped",
		ResolvedAt: "2026-01-01T00:00:00Z", ResolvedBy: "u-old",
		Dossier: &sessionstore.Dossier{Summary: "keep me", NextActions: []string{"retry"}},
		Label:   sessionstore.LabelDone,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReopenCase(tid, "u1", "shipping"); err != ErrCaseBadPhase {
		t.Fatalf("bad phase: got %v", err)
	}
	e, _ = store.Get(tid)
	if !e.IsCaseClosed() || e.Resolution != "fixed" {
		t.Fatalf("bad phase must not mutate: %+v", e)
	}
	if err := b.ReopenCase(tid, "u1", "fixing"); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get(tid)
	if e.Phase != sessionstore.PhaseFixing || e.Label != sessionstore.LabelInProgress {
		t.Fatalf("reopen fixing: phase=%q label=%q", e.Phase, e.Label)
	}
	if e.Resolution != "" || e.ResolvedBy != "" {
		t.Fatalf("resolution not cleared: %+v", e)
	}
	if e.Dossier == nil || e.Dossier.Summary != "keep me" || len(e.Dossier.NextActions) != 1 {
		t.Fatalf("dossier: %+v", e.Dossier)
	}
	if e.IsCaseClosed() {
		t.Fatal("still closed")
	}
	// Already open (just reopened) → clear error, no clobber.
	if err := b.ReopenCase(tid, "u1", "investigate"); err != ErrCaseNotClosed {
		t.Fatalf("reopen already-open: got %v", err)
	}
	e, _ = store.Get(tid)
	if e.Phase != sessionstore.PhaseFixing {
		t.Fatalf("clobbered open phase to %q", e.Phase)
	}
}

func TestCanEscalateCaseCaps(t *testing.T) {
	if CanEscalateCaseCaps(config.Capabilities{Investigate: true}) {
		t.Fatal("investigate alone cannot escalate")
	}
	if !CanEscalateCaseCaps(config.Capabilities{FileEscalation: true}) {
		t.Fatal("fileEscalation should escalate")
	}
	if !CanDraftCaseCaps(config.Capabilities{DraftCustomerReply: true}) {
		t.Fatal("draft should draft")
	}
	if !CanReopenCaseCaps(config.Capabilities{Investigate: true}) {
		t.Fatal("investigate should reopen")
	}
	if !CanReopenCaseCaps(config.Capabilities{FileEscalation: true}) {
		t.Fatal("fileEscalation should reopen")
	}
	if !CanReopenCaseCaps(config.Capabilities{StartSessions: true}) {
		t.Fatal("startSessions should reopen")
	}
	if CanReopenCaseCaps(config.Capabilities{DraftCustomerReply: true}) {
		t.Fatal("draft alone should not reopen")
	}
}
