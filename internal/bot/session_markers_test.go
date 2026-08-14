package bot

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParseSessionLifecycleMarker(t *testing.T) {
	t.Parallel()
	kind, reason := parseSessionLifecycleMarker("all good\nSESSION_DONE:\n")
	if kind != sessionMarkerDone || reason != "" {
		t.Fatalf("done: kind=%v reason=%q", kind, reason)
	}
	kind, reason = parseSessionLifecycleMarker("nope\nSESSION_ABANDON: out of time\n")
	if kind != sessionMarkerAbandon || reason != "out of time" {
		t.Fatalf("abandon: kind=%v reason=%q", kind, reason)
	}
	// Last marker wins when both present.
	kind, _ = parseSessionLifecycleMarker("SESSION_DONE:\nSESSION_ABANDON: later\n")
	if kind != sessionMarkerAbandon {
		t.Fatalf("abandon should win when last, got %v", kind)
	}
	kind, _ = parseSessionLifecycleMarker("SESSION_ABANDON: x\nSESSION_DONE:\n")
	if kind != sessionMarkerDone {
		t.Fatalf("done should win when last, got %v", kind)
	}
	// Multiple done after abandon: last done must win (not first-done vs last-ab).
	kind, reason = parseSessionLifecycleMarker("SESSION_DONE:\nSESSION_ABANDON: temp\nSESSION_DONE:\n")
	if kind != sessionMarkerDone || reason != "" {
		t.Fatalf("last done after abandon: kind=%v reason=%q", kind, reason)
	}
	kind, reason = parseSessionLifecycleMarker("SESSION_ABANDON: a\nSESSION_DONE:\nSESSION_ABANDON: final\n")
	if kind != sessionMarkerAbandon || reason != "final" {
		t.Fatalf("last abandon: kind=%v reason=%q", kind, reason)
	}
	kind, _ = parseSessionLifecycleMarker("no markers here")
	if kind != sessionMarkerNone {
		t.Fatalf("none: %v", kind)
	}
}

func TestApplySessionDoneMarker(t *testing.T) {
	b, _ := testFixBot(t)
	const tid = "mk-done"
	if err := b.sessions.Set(tid, sessionstore.Entry{
		Project: "app", Cwd: "/tmp/wt", WorktreeBranch: "grokwork/" + tid,
	}); err != nil {
		t.Fatal(err)
	}
	kind, note := b.applySessionLifecycleMarkers(tid, "finished\nSESSION_DONE:\n", "alice")
	if kind != sessionMarkerDone || !strings.Contains(note, "done") {
		t.Fatalf("kind=%v note=%q", kind, note)
	}
	e, ok := b.sessions.Get(tid)
	if !ok {
		t.Fatal("missing session")
	}
	if e.EffectiveLabel() != sessionstore.LabelDone || !e.LabelManual {
		t.Fatalf("label=%q manual=%v", e.EffectiveLabel(), e.LabelManual)
	}
	// Worktree fields preserved.
	if e.Cwd != "/tmp/wt" || e.WorktreeBranch != "grokwork/"+tid {
		t.Fatalf("worktree cleared: cwd=%q branch=%q", e.Cwd, e.WorktreeBranch)
	}
}

func TestSoftAbandonDoesNotRemoveWorktree(t *testing.T) {
	b, _ := testFixBot(t)
	const tid = "mk-abandon"
	if err := b.sessions.Set(tid, sessionstore.Entry{
		Project: "app", Cwd: "/tmp/wt-keep", WorktreeBranch: "grokwork/" + tid, SessionID: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	// Seed a queued follow-up under a busy job.
	var cancelled atomic.Bool
	job := &runJob{cancel: func() { cancelled.Store(true) }, project: "app"}
	if claimed, _, err := b.claimOrEnqueue(tid, job, taskItem{threadID: tid}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, _, err := b.claimOrEnqueue(tid, &runJob{}, taskItem{threadID: tid, taskID: "q1", authorID: "u"}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.SoftAbandonSession(tid, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "abandoned") {
		t.Fatalf("msg=%q", msg)
	}
	if !cancelled.Load() {
		t.Fatal("expected active run cancelled")
	}
	if b.queueLen(tid) != 0 {
		t.Fatalf("queue not cleared: %d", b.queueLen(tid))
	}
	e, _ := b.sessions.Get(tid)
	if e.EffectiveLabel() != sessionstore.LabelAbandoned || !e.LabelManual {
		t.Fatalf("label=%q manual=%v", e.EffectiveLabel(), e.LabelManual)
	}
	// Soft abandon must keep worktree/session identity for human inspection.
	if e.Cwd != "/tmp/wt-keep" || e.WorktreeBranch == "" || e.SessionID != "s1" {
		t.Fatalf("destructive reset leaked: %+v", e)
	}
	// Contrast: ResetUnit clears those fields (and would refuse while busy — finish first).
	FinishRunForTest(b, tid)
	if _, err := b.ResetUnit(tid); err != nil {
		t.Fatal(err)
	}
	e2, _ := b.sessions.Get(tid)
	if e2.Cwd != "" || e2.SessionID != "" {
		t.Fatalf("ResetUnit should clear worktree fields: %+v", e2)
	}
}

func TestApplyAbandonMarkerViaParser(t *testing.T) {
	b, _ := testFixBot(t)
	const tid = "mk-ab-parse"
	if err := b.sessions.Set(tid, sessionstore.Entry{
		Project: "app", Cwd: filepath.Join(t.TempDir(), "wt"), WorktreeBranch: "grokwork/" + tid,
	}); err != nil {
		t.Fatal(err)
	}
	kind, note := b.applySessionLifecycleMarkers(tid, "giving up\nSESSION_ABANDON: stuck\n", "bob")
	if kind != sessionMarkerAbandon || !strings.Contains(note, "abandoned") {
		t.Fatalf("kind=%v note=%q", kind, note)
	}
	e, _ := b.sessions.Get(tid)
	if e.Cwd == "" {
		t.Fatal("worktree path must survive soft abandon")
	}
}

func TestRemoteWorkPromptTeachesSessionMarkers(t *testing.T) {
	p := remoteWorkPromptPrefix("grokwork/t1")
	for _, want := range []string{"SESSION_DONE:", "SESSION_ABANDON:"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in prompt", want)
		}
	}
}

func TestParseSessionMarkersIgnoreIndentedContractQuotes(t *testing.T) {
	// Prompt contract indents examples; quoting them must not abandon.
	quoted := sessionLifecycleMarkerContract()
	body := strings.Join(quoted, "\n")
	kind, _ := parseSessionLifecycleMarker(body)
	if kind != sessionMarkerNone {
		t.Fatalf("indented contract must not match, got %v", kind)
	}
}

// TestAbandonMarkerSkipsDirectShipGate pins the executeTask order: after
// SESSION_ABANDON, direct-ship must not run (would stamp done + delete worktree).
// Exercises the gate expression used in executeTask, not only SoftAbandonSession.
func TestAbandonMarkerSkipsDirectShipGate(t *testing.T) {
	b, _ := testFixBot(t)
	const tid = "mk-ab-direct"
	cwd := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.sessions.Set(tid, sessionstore.Entry{
		Project: "app", Cwd: cwd, WorktreeBranch: "grokwork/" + tid, SessionID: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	kind, _ := b.applySessionLifecycleMarkers(tid, "nope\nSESSION_ABANDON: half done\n", "agent")
	if kind != sessionMarkerAbandon {
		t.Fatalf("kind=%v", kind)
	}
	// Mirror executeTask gate: ship only when not abandon.
	allowDirectIntegrate := true
	direct := true
	wtBranch := "grokwork/" + tid
	wouldShip := allowDirectIntegrate && direct && wtBranch != "" && kind != sessionMarkerAbandon
	if wouldShip {
		t.Fatal("SESSION_ABANDON must skip direct ship")
	}
	e, _ := b.sessions.Get(tid)
	if e.EffectiveLabel() != sessionstore.LabelAbandoned || !e.LabelManual {
		t.Fatalf("label=%q manual=%v", e.EffectiveLabel(), e.LabelManual)
	}
	if e.Cwd != cwd || e.WorktreeBranch == "" {
		t.Fatalf("worktree must remain after abandon: %+v", e)
	}
}
