package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleRun(id string) Run {
	return Run{
		ID: id, Project: "shop", Repo: "acme/api", Service: "api", Env: "prod",
		Ref: "main", SHA: "abcdef1234567890", ShortSHA: "abcdef1",
		Status: StatusPending,
		Steps: []StepRecord{
			{Name: "build", Command: "docker build .", TimeoutMs: 60_000, Status: StatusPending},
			{Name: "rollout", Command: "kubectl apply", TimeoutMs: 60_000, Status: StatusPending},
		},
	}
}

func TestStoreCreateGetUpdate(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	if err := s.Create(sampleRun(id)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(id)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d", got.SchemaVersion)
	}
	if got.QueuedAt == "" {
		t.Fatal("QueuedAt not stamped")
	}
	if got.Lane() != "shop/acme/api/api/prod" {
		t.Fatalf("Lane = %q", got.Lane())
	}

	if err := s.Update(id, func(r *Run) error {
		r.Status = StatusRunning
		r.Steps[0].Status = StatusSucceeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get(id)
	if got.Status != StatusRunning || got.Steps[0].Status != StatusSucceeded {
		t.Fatalf("update lost: %+v", got)
	}
}

func TestStoreUpdateSkip(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	if err := s.Create(sampleRun(id)); err != nil {
		t.Fatal(err)
	}
	err := s.Update(id, func(r *Run) error {
		r.Status = StatusFailed
		return ErrSkipUpdate
	})
	if err != nil {
		t.Fatalf("Update returned %v, want nil for a skip", err)
	}
	got, _, _ := s.Get(id)
	if got.Status != StatusPending {
		t.Fatalf("ErrSkipUpdate still wrote: status = %q", got.Status)
	}
}

func TestStoreUpdateErrorDoesNotWrite(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	if err := s.Create(sampleRun(id)); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("nope")
	if err := s.Update(id, func(r *Run) error {
		r.Status = StatusFailed
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Update err = %v, want the callback's", err)
	}
	got, _, _ := s.Get(id)
	if got.Status != StatusPending {
		t.Fatalf("a failed callback still persisted: %q", got.Status)
	}
}

func TestStoreUpdateUnknownRun(t *testing.T) {
	s := testStore(t)
	if err := s.Update(NewRunID(), func(*Run) error { return nil }); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

// TestStoreRejectsBadRunID guards the path: run ids reach the filesystem, so an
// id that is not the minted shape must never be joined into a path.
func TestStoreRejectsBadRunID(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"../escape", "d_short", "", "d_" + strings.Repeat("g", 32), "/abs"} {
		if err := s.Create(Run{ID: id}); err == nil {
			t.Errorf("Create accepted id %q", id)
		}
		if _, _, err := s.Get(id); err == nil {
			t.Errorf("Get accepted id %q", id)
		}
		if _, err := s.StepLogPath(id, 0, "s"); err == nil {
			t.Errorf("StepLogPath accepted id %q", id)
		}
	}
}

func TestNewRunIDIsUniqueAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		id := NewRunID()
		if !runIDRe.MatchString(id) {
			t.Fatalf("malformed id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestStoreListAndLane(t *testing.T) {
	s := testStore(t)
	a, b, other := NewRunID(), NewRunID(), NewRunID()
	ra, rb := sampleRun(a), sampleRun(b)
	ra.QueuedAt = "2026-07-01T00:00:00Z"
	rb.QueuedAt = "2026-07-02T00:00:00Z"
	ro := sampleRun(other)
	ro.Env = "dev"
	ro.QueuedAt = "2026-07-03T00:00:00Z"
	for _, r := range []Run{ra, rb, ro} {
		if err := s.Create(r); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List = %d runs, want 3", len(all))
	}
	// Newest queued first.
	if all[0].ID != other {
		t.Fatalf("List not newest-first: %s", all[0].ID)
	}

	lane, err := s.ListForLane("shop/acme/api/api/prod", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lane) != 2 {
		t.Fatalf("ListForLane = %d, want 2 (dev is a different lane)", len(lane))
	}
	if lane[0].ID != b {
		t.Fatalf("lane not newest-first: %s", lane[0].ID)
	}
	limited, err := s.ListForLane("shop/acme/api/api/prod", 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit ignored: %d %v", len(limited), err)
	}
}

// TestStoreListSkipsJunk pins that a stray file or a torn record cannot break
// the whole listing — which on the deploys page would mean an empty board.
func TestStoreListSkipsJunk(t *testing.T) {
	s := testStore(t)
	good := NewRunID()
	if err := s.Create(sampleRun(good)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "d_"+strings.Repeat("a", 32)+".json"), []byte("{tor"), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatalf("List failed on junk: %v", err)
	}
	if len(all) != 1 || all[0].ID != good {
		t.Fatalf("List = %+v, want just the good run", all)
	}
}

func TestStoreStepLogRoundTrip(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	if err := s.Create(sampleRun(id)); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateStepLog(id, 0, "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line one\n"); err != nil {
		t.Fatal(err)
	}

	// Tail from the start.
	chunk, off, err := s.ReadStepLog(id, 0, "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "line one\n" || off != 9 {
		t.Fatalf("chunk=%q off=%d", chunk, off)
	}
	// Nothing new yet.
	chunk, off2, err := s.ReadStepLog(id, 0, "build", off)
	if err != nil || len(chunk) != 0 || off2 != off {
		t.Fatalf("chunk=%q off=%d err=%v", chunk, off2, err)
	}
	// Append and resume from the offset.
	if _, err := f.WriteString("line two\n"); err != nil {
		t.Fatal(err)
	}
	chunk, _, err = s.ReadStepLog(id, 0, "build", off)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "line two\n" {
		t.Fatalf("resume chunk = %q", chunk)
	}
	_ = f.Close()
}

func TestReadStepLogMissingIsNotAnError(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	// A step that has not started has no log yet; the page asks for it anyway.
	chunk, off, err := s.ReadStepLog(id, 3, "later", 0)
	if err != nil || len(chunk) != 0 || off != 0 {
		t.Fatalf("chunk=%q off=%d err=%v", chunk, off, err)
	}
}

func TestReadStepLogHandlesShrunkFile(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	f, err := s.CreateStepLog(id, 0, "build")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("short\n")
	_ = f.Close()
	// Offset past the end (file replaced by a shorter one) restarts rather than
	// seeking past EOF and returning nonsense.
	chunk, off, err := s.ReadStepLog(id, 0, "build", 9999)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "short\n" || off != 6 {
		t.Fatalf("chunk=%q off=%d", chunk, off)
	}
}

func TestStepLogPathSanitizesName(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	p, err := s.StepLogPath(id, 2, "Roll Out/../etc")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(p)
	if strings.Contains(base, "/") || strings.Contains(base, "..") {
		t.Fatalf("unsanitized step log name: %q", base)
	}
	if !strings.HasPrefix(base, "02-") {
		t.Fatalf("step index missing from %q", base)
	}
}

func TestStoreDeleteRemovesLogs(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	if err := s.Create(sampleRun(id)); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateStepLog(id, 0, "build")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("x")
	_ = f.Close()

	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(id); ok {
		t.Fatal("record survived Delete")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), id)); !os.IsNotExist(err) {
		t.Fatal("log directory survived Delete")
	}
}

// TestStoreConcurrentUpdates runs with -race: Update is the only mutation point
// and must serialise, or two steps finishing at once lose one of the writes.
func TestStoreConcurrentUpdates(t *testing.T) {
	s := testStore(t)
	id := NewRunID()
	r := sampleRun(id)
	r.Steps = make([]StepRecord, 20)
	for i := range r.Steps {
		r.Steps[i] = StepRecord{Name: "s", Status: StatusPending}
	}
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_ = s.Update(id, func(run *Run) error {
				run.Steps[i].Status = StatusSucceeded
				return nil
			})
		})
	}
	wg.Wait()
	got, _, _ := s.Get(id)
	for i, st := range got.Steps {
		if st.Status != StatusSucceeded {
			t.Fatalf("step %d lost its update: %+v", i, st)
		}
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusRunning, StatusCancelling} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestLaneKeyRepoless(t *testing.T) {
	// A project with no GitHub catalog still needs a stable lane segment.
	if got := LaneKey("p", "", "api", "dev"); got != "p/./api/dev" {
		t.Fatalf("LaneKey = %q", got)
	}
}
