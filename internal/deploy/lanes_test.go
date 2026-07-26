package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// createAt writes a run and pins its mtime, so the newest-first scan order is
// the test's choice and not the filesystem's timestamp resolution.
func createAt(t *testing.T, s *Store, r Run, mod time.Time) {
	t.Helper()
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir(), r.ID+".json")
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func laneRun(id, project, service, env, sha, status string, queuedAt string) Run {
	r := sampleRun(id)
	r.Project = project
	r.Service = service
	r.Env = env
	r.SHA = sha
	r.ShortSHA = sha
	r.Status = Status(status)
	r.QueuedAt = queuedAt
	return r
}

func findLane(t *testing.T, states []LaneState, project, service, env string) LaneState {
	t.Helper()
	for _, st := range states {
		if st.Project == project && st.Service == service && st.Env == env {
			return st
		}
	}
	t.Fatalf("lane %s/%s/%s missing from %d states", project, service, env, len(states))
	return LaneState{}
}

// TestLaneStatesCurrentIsUnsupersededSuccess pins the definition of "what is
// running here": the newest success that no later success replaced. A
// superseded record and a failed attempt on top of a good deploy must both
// leave Current alone — otherwise the board would name a commit that is not
// live, which is worse than naming none.
func TestLaneStatesCurrentIsUnsupersededSuccess(t *testing.T) {
	s := testStore(t)
	base := time.Now().Add(-time.Hour)

	old := laneRun(NewRunID(), "shop", "api", "prod", "aaaaaaa", "succeeded", "2026-07-01T00:00:00Z")
	old.SupersededBy = "later"
	live := laneRun(NewRunID(), "shop", "api", "prod", "bbbbbbb", "succeeded", "2026-07-02T00:00:00Z")
	failed := laneRun(NewRunID(), "shop", "api", "prod", "ccccccc", "failed", "2026-07-03T00:00:00Z")
	createAt(t, s, old, base)
	createAt(t, s, live, base.Add(time.Minute))
	createAt(t, s, failed, base.Add(2*time.Minute))

	states, truncated, err := s.LaneStates(0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("three runs must not clip the default scan")
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want one lane: %+v", len(states), states)
	}
	st := states[0]
	if !st.HasCurrent || st.Current.ID != live.ID {
		t.Fatalf("Current = %+v, want the unsuperseded success %s", st.Current, live.ID)
	}
	if st.Latest.ID != failed.ID {
		t.Fatalf("Latest = %s, want the newest run %s", st.Latest.ID, failed.ID)
	}
	if st.LatestIsCurrent() {
		t.Fatal("a failed attempt on top of a good deploy must not read as settled")
	}
	if st.Lane != live.Lane() {
		t.Fatalf("Lane = %q, want %q", st.Lane, live.Lane())
	}
}

// TestLaneStatesNeverPromotesASupersededRun is why "current" reads the stored
// flag instead of re-deriving "newest success", and why the difference is not
// theoretical.
//
// Engine.markSuperseded rewrites the *older* record after the new one has
// finished, so a superseded record is the more recently touched file of the
// pair. The scan takes newest-touched first, so a clip can land between them
// and leave only the superseded record in the window. Trusting recency there
// would print a commit that was replaced as the one running in production —
// with no hint that anything was omitted beyond the truncation note.
func TestLaneStatesNeverPromotesASupersededRun(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	live := laneRun(NewRunID(), "shop", "api", "prod", "11ve777", "succeeded", "2026-07-02T00:00:00Z")
	live.EndedAt = "2026-07-02T00:05:00Z"
	replaced := laneRun(NewRunID(), "shop", "api", "prod", "0ldddd0", "succeeded", "2026-07-01T00:00:00Z")
	replaced.EndedAt = "2026-07-01T00:05:00Z"
	replaced.SupersededBy = live.ID

	createAt(t, s, live, now)
	// Stamped superseded a moment after the newer run's own record was written.
	createAt(t, s, replaced, now.Add(time.Second))

	states, truncated, err := s.LaneStates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("a one-record scan of two records must report truncated")
	}
	st := findLane(t, states, "shop", "api", "prod")
	if st.HasCurrent {
		t.Fatalf("a superseded run was promoted to current: %s (%s)",
			st.Current.ID, st.Current.ShortSHA)
	}
	// It is also not something to chase: it succeeded, it is just not live.
	if st.Unsettled() {
		t.Fatal("a superseded success must not read as an unfinished attempt")
	}
	// Reading the whole store finds the live run again — nothing was lost.
	all, _, err := s.LaneStates(0)
	if err != nil {
		t.Fatal(err)
	}
	full := findLane(t, all, "shop", "api", "prod")
	if !full.HasCurrent || full.Current.ID != live.ID {
		t.Fatalf("unbounded scan Current = %+v, want %s", full.Current, live.ID)
	}
	if full.Latest.ID != live.ID || !full.LatestIsCurrent() {
		t.Fatalf("Latest = %s, want the newest-queued run %s", full.Latest.ID, live.ID)
	}
}

// TestLaneStatesNoSuccessStillReportsLane covers the lane whose every run
// failed. Dropping it would render as "this environment does not exist", when
// the truth — nothing has ever shipped here — is the most important thing the
// board can say.
func TestLaneStatesNoSuccessStillReportsLane(t *testing.T) {
	s := testStore(t)
	bad := laneRun(NewRunID(), "shop", "api", "dev", "ddddddd", "failed", "2026-07-01T00:00:00Z")
	createAt(t, s, bad, time.Now())

	states, _, err := s.LaneStates(0)
	if err != nil {
		t.Fatal(err)
	}
	st := findLane(t, states, "shop", "api", "dev")
	if st.HasCurrent {
		t.Fatalf("HasCurrent on a lane that never succeeded: %+v", st.Current)
	}
	if st.Latest.ID != bad.ID {
		t.Fatalf("Latest = %s, want %s", st.Latest.ID, bad.ID)
	}
	if st.LatestIsCurrent() {
		t.Fatal("LatestIsCurrent must be false with no current deploy")
	}
}

// TestLaneStatesSeparatesLanes pins that project, service and environment each
// key a distinct lane, and that the result is ordered so the board is stable
// across renders.
func TestLaneStatesSeparatesLanes(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	runs := []Run{
		laneRun(NewRunID(), "shop", "web", "prod", "1111111", "succeeded", "2026-07-01T00:00:00Z"),
		laneRun(NewRunID(), "shop", "api", "prod", "2222222", "succeeded", "2026-07-01T00:00:00Z"),
		laneRun(NewRunID(), "shop", "api", "dev", "3333333", "succeeded", "2026-07-01T00:00:00Z"),
		laneRun(NewRunID(), "blog", "api", "prod", "4444444", "succeeded", "2026-07-01T00:00:00Z"),
	}
	for i, r := range runs {
		createAt(t, s, r, now.Add(time.Duration(i)*time.Minute))
	}
	states, _, err := s.LaneStates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 4 {
		t.Fatalf("states = %d, want 4 lanes", len(states))
	}
	var got []string
	for _, st := range states {
		got = append(got, st.Project+"/"+st.Service+"/"+st.Env)
	}
	want := []string{"blog/api/prod", "shop/api/dev", "shop/api/prod", "shop/web/prod"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestLaneStatesBoundsTheScan is the reason this lives in the store rather than
// as a List() filter: the board must not open every record ever written. The
// clip is newest-first, so the runs that fall outside it are the stale ones.
func TestLaneStatesBoundsTheScan(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	// Ten lanes, one run each, oldest first.
	ids := make([]string, 10)
	for i := range ids {
		r := laneRun(NewRunID(), "shop", "svc"+string(rune('a'+i)), "prod",
			"sha", "succeeded", "2026-07-01T00:00:00Z")
		ids[i] = r.ID
		createAt(t, s, r, now.Add(time.Duration(i)*time.Minute))
	}

	states, truncated, err := s.LaneStates(3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("clipping 10 records to 3 must report truncated")
	}
	if len(states) != 3 {
		t.Fatalf("states = %d, want 3 (one lane per scanned record)", len(states))
	}
	// The three most recently touched records are the last three written.
	for _, st := range states {
		switch st.Service {
		case "svch", "svci", "svcj":
		default:
			t.Fatalf("scan kept a stale lane %q instead of the newest three", st.Service)
		}
	}
	// The unbounded read still sees everything, so nothing is lost on disk.
	all, truncated, err := s.LaneStates(1000)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(all) != 10 {
		t.Fatalf("unbounded scan = %d lanes truncated=%v, want 10 false", len(all), truncated)
	}
}

// TestLaneStatesSkipsJunk mirrors TestStoreListSkipsJunk: on the board, one
// torn record must not blank out every project's deploy state.
func TestLaneStatesSkipsJunk(t *testing.T) {
	s := testStore(t)
	good := laneRun(NewRunID(), "shop", "api", "prod", "eeeeeee", "succeeded", "2026-07-01T00:00:00Z")
	createAt(t, s, good, time.Now())
	if err := os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(s.Dir(), "d_"+strings.Repeat("a", 32)+".json")
	if err := os.WriteFile(torn, []byte("{tor"), 0o600); err != nil {
		t.Fatal(err)
	}
	states, _, err := s.LaneStates(0)
	if err != nil {
		t.Fatalf("LaneStates failed on junk: %v", err)
	}
	if len(states) != 1 || states[0].Current.ID != good.ID {
		t.Fatalf("states = %+v, want just the good run", states)
	}
}

func TestLaneStatesEmptyStore(t *testing.T) {
	s := testStore(t)
	states, truncated, err := s.LaneStates(0)
	if err != nil || truncated || len(states) != 0 {
		t.Fatalf("empty store = %+v truncated=%v err=%v", states, truncated, err)
	}
}
