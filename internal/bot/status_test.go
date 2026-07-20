package bot

import (
	"context"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStatusSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("thread-a", sessionstore.Entry{SessionID: "sid", Project: "app"}); err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": dir, "api": dir}),
		AllowedUsers: map[string]struct{}{
			"u1": {},
		},
		AllowedRoles: map[string]struct{}{
			"r1": {},
		},
		AllowedUserIDs: []string{"u1"},
		AllowedRoleIDs: []string{"r1"},
		DataDir:        dir,
	}
	b := New(cfg, store, hist)

	// Inject an active run via claimOrEnqueue (real bot path).
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &runJob{cancel: cancel, start: time.Now().Add(-3 * time.Second), project: "app"}
	claimed, _, err := b.claimOrEnqueue("thread-run", job, taskItem{threadID: "thread-run"})
	if err != nil || !claimed {
		t.Fatalf("claimOrEnqueue: claimed=%v err=%v", claimed, err)
	}
	// Queue a follow-up.
	_, pos, err := b.claimOrEnqueue("thread-run", &runJob{}, taskItem{threadID: "thread-run"})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Fatalf("queue pos=%d", pos)
	}

	snap := b.StatusSnapshot()
	if snap.SessionCount != 1 {
		t.Fatalf("SessionCount=%d", snap.SessionCount)
	}
	if snap.ProjectCount != 2 {
		t.Fatalf("ProjectCount=%d", snap.ProjectCount)
	}
	if snap.AllowUsers != 1 || snap.AllowRoles != 1 {
		t.Fatalf("allow users=%d roles=%d", snap.AllowUsers, snap.AllowRoles)
	}
	if snap.ActiveCount != 1 || len(snap.ActiveRuns) != 1 {
		t.Fatalf("active: count=%d runs=%+v", snap.ActiveCount, snap.ActiveRuns)
	}
	run := snap.ActiveRuns[0]
	if run.ThreadID != "thread-run" || run.Project != "app" || run.QueueLen != 1 {
		t.Fatalf("run=%+v", run)
	}
	if run.Elapsed == "" {
		t.Fatal("expected elapsed string")
	}
	if snap.QueuedTotal != 1 {
		t.Fatalf("QueuedTotal=%d", snap.QueuedTotal)
	}
	if snap.Time.IsZero() {
		t.Fatal("Time zero")
	}
}
