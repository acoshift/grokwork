package bot

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// First human submit on a brand-new unit mints a shell with a turn stamp so
// boards/catchup see activity before the agent finishes.
func TestTouchSessionTurnCreatesShell(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:  dir,
		Projects: config.PathProjects(map[string]string{"app": filepath.Join(dir, "app")}),
	}
	b := New(cfg, store, nil)
	threadID := "new-thread-1"
	if _, ok := store.Get(threadID); ok {
		t.Fatal("expected no session yet")
	}
	job := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	item := taskItem{
		threadID: threadID,
		proj:     projectRef{Name: "app", Cwd: filepath.Join(dir, "app")},
		actor:    Actor{ID: "u1", DisplayName: "Alice"},
		source:   SourceDiscord,
		origin:   SourceDiscord,
	}
	claimed, _, err := b.claimOrEnqueue(threadID, job, item)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	e, ok := store.Get(threadID)
	if !ok {
		t.Fatal("expected shell session after human submit")
	}
	if e.UpdatedAt == "" {
		t.Fatal("shell missing UpdatedAt")
	}
	if e.Project != "app" || e.LastUser != "Alice" {
		t.Fatalf("shell: %+v", e)
	}
	if e.OwnerID != "u1" {
		t.Fatalf("OwnerID=%q", e.OwnerID)
	}
	// Recovery rehydrate must not re-stamp.
	before := e.UpdatedAt
	time.Sleep(10 * time.Millisecond)
	job2 := &runJob{cancel: func() {}, start: time.Now(), project: "app"}
	// Thread already has a job — enqueue via skipReady path after finish is awkward.
	// Call touch-skip path by claimOrEnqueueInternal with skipReady on a free thread.
	b.finishRun(threadID) // clear job so we can reclaim
	claimed, _, err = b.claimOrEnqueueInternal(threadID, job2, item, true)
	if err != nil || !claimed {
		t.Fatalf("resume claim: claimed=%v err=%v", claimed, err)
	}
	after, _ := store.Get(threadID)
	if after.UpdatedAt != before {
		t.Fatalf("recovery rehydrate stamped UpdatedAt %q → %q", before, after.UpdatedAt)
	}
}

func TestResetUnitStampsTerminalTurn(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	oldAt := "2020-06-01T00:00:00Z"
	if err := store.Set("t-abandon", sessionstore.Entry{
		SessionID: "s1",
		Project:   "app",
		UpdatedAt: oldAt,
		LastUser:  "bob",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ResetUnit("t-abandon"); err != nil {
		t.Fatal(err)
	}
	e, ok := store.Get("t-abandon")
	if !ok {
		t.Fatal("tombstone missing")
	}
	if e.EffectiveLabel() != sessionstore.LabelAbandoned {
		t.Fatalf("label=%q", e.EffectiveLabel())
	}
	if e.UpdatedAt == "" || e.UpdatedAt == oldAt {
		t.Fatalf("abandon must stamp UpdatedAt, got %q", e.UpdatedAt)
	}
}

func TestSetSessionLabelTerminalStamps(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	b := New(cfg, store, nil)
	oldAt := "2021-01-01T00:00:00Z"
	if err := store.Set("t-done", sessionstore.Entry{
		Project: "app", UpdatedAt: oldAt, LastUser: "carol",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSessionLabel("t-done", sessionstore.LabelDone); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get("t-done")
	if e.UpdatedAt == "" || e.UpdatedAt == oldAt {
		t.Fatalf("done label must stamp UpdatedAt, got %q", e.UpdatedAt)
	}
	// Non-terminal must not stamp.
	if err := store.Set("t-wip", sessionstore.Entry{
		Project: "app", UpdatedAt: oldAt, LastUser: "carol",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSessionLabel("t-wip", sessionstore.LabelInProgress); err != nil {
		t.Fatal(err)
	}
	e, _ = store.Get("t-wip")
	if e.UpdatedAt != oldAt {
		t.Fatalf("non-terminal label moved UpdatedAt %q → %q", oldAt, e.UpdatedAt)
	}
}
