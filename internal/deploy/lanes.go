package deploy

import (
	"cmp"
	"os"
	"slices"
	"strings"
	"time"
)

// DefaultLaneScanLimit bounds how many run records one lane fold parses.
//
// Nothing prunes data/deploys/runs, so records accumulate for the life of the
// installation. A cross-project board that parsed all of them would get slower
// forever — and it re-renders on every SSE tick, for every open tab. 500 is far
// more than an operator deploys in a working week across every lane, and
// LaneStates reports when it clipped so the page can say the board covers
// recent history rather than silently showing a partial one.
const DefaultLaneScanLimit = 500

// LaneState is one deploy lane's standing: what is live on it, and what
// happened to it most recently.
type LaneState struct {
	Lane    string
	Project string
	Repo    string
	Service string
	Env     string

	// Current is what is deployed: the newest succeeded run that no later
	// success has replaced. "Replaced" is exactly what the per-project board
	// prints as "superseded" — Engine.markSuperseded stamps SupersededBy on
	// every older success when a new one lands — so the two surfaces cannot
	// disagree about which commit is live.
	Current    Run
	HasCurrent bool

	// Latest is the newest run on the lane whatever its status. On a quiet lane
	// it is Current; when it is not, something has happened since the live
	// commit shipped — a deploy in flight, or a failed attempt on top of it.
	Latest Run
}

// LatestIsCurrent reports whether the newest run on the lane is the live one,
// i.e. nothing has been attempted since the current deploy succeeded.
func (l LaneState) LatestIsCurrent() bool {
	return l.HasCurrent && l.Latest.ID == l.Current.ID
}

// Unsettled reports whether the lane's newest run is something an operator
// still has to look at: a deploy in flight, or an attempt that did not succeed.
//
// A *succeeded* run that is not the current one does not count. That happens
// when two runs on a lane finish out of order — the later-queued one lands
// first and the engine stamps it superseded — and surfacing it would put a
// green "succeeded" chip next to a different commit, which reads as a
// contradiction rather than as news.
func (l LaneState) Unsettled() bool {
	return !l.LatestIsCurrent() && l.Latest.Status != StatusSucceeded
}

// LaneStates folds recent run records into one summary per lane, ordered by
// project, service, environment.
//
// maxScan bounds how many records are opened and parsed; <= 0 means
// DefaultLaneScanLimit. Candidates are taken newest-modified first, because
// mtime is the only recency signal available without opening a file: a record
// is rewritten on create, on every status transition, and when a later success
// supersedes it, so the newest-touched prefix is precisely the recently active
// lanes. Reading a directory costs one stat per entry; parsing costs a read
// plus a JSON decode, and it is the parse that this bounds.
//
// truncated reports that older records were left unread. A lane last deployed
// long ago can then be missing from the result entirely, which a caller must
// say out loud rather than render as "never deployed".
func (s *Store) LaneStates(maxScan int) (states []LaneState, truncated bool, err error) {
	if maxScan <= 0 {
		maxScan = DefaultLaneScanLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	type candidate struct {
		id  string
		mod time.Time
	}
	cands := make([]candidate, 0, len(entries))
	for _, e := range entries {
		// Per-run log directories live alongside the records.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c := candidate{id: strings.TrimSuffix(e.Name(), ".json")}
		if info, iErr := e.Info(); iErr == nil {
			c.mod = info.ModTime()
		}
		// A record that vanished between ReadDir and Info keeps a zero mtime and
		// sorts last; loadLocked then skips it.
		cands = append(cands, c)
	}
	// Newest first. The id breaks ties so the clip point is deterministic when a
	// burst of runs shares a coarse filesystem timestamp.
	slices.SortFunc(cands, func(a, b candidate) int {
		return cmp.Or(b.mod.Compare(a.mod), strings.Compare(b.id, a.id))
	})
	if len(cands) > maxScan {
		cands = cands[:maxScan]
		truncated = true
	}

	byLane := make(map[string]*LaneState, len(cands))
	for _, c := range cands {
		r, ok, lErr := s.loadLocked(c.id)
		if lErr != nil || !ok {
			// A torn or foreign file must not empty the whole board.
			continue
		}
		lane := r.Lane()
		st := byLane[lane]
		if st == nil {
			st = &LaneState{
				Lane: lane, Project: r.Project, Repo: r.Repo,
				Service: r.Service, Env: r.Env,
				Latest: r,
			}
			byLane[lane] = st
		} else if newerRun(r, st.Latest) {
			st.Latest = r
		}
		// SupersededBy is consulted rather than re-derived. "Newest success" is
		// a different rule that usually agrees and sometimes does not: two runs
		// can queue on one lane and finish in the other order, leaving the
		// later-queued one superseded by the one that actually landed last.
		// Recomputing here would then name a commit the per-project page prints
		// as "superseded", and the two surfaces would disagree about what is
		// running. The stored flag is the fact; this only picks between flags.
		if r.Status == StatusSucceeded && r.SupersededBy == "" &&
			(!st.HasCurrent || finishedLater(r, st.Current)) {
			st.Current = r
			st.HasCurrent = true
		}
	}

	states = make([]LaneState, 0, len(byLane))
	for _, st := range byLane {
		states = append(states, *st)
	}
	slices.SortFunc(states, func(a, b LaneState) int {
		return cmp.Or(
			strings.Compare(a.Project, b.Project),
			strings.Compare(a.Service, b.Service),
			// Environments sort by name: their declared order lives in the
			// manifest, and reading one costs a git call per project, which is
			// the whole reason this fold reads the run store instead.
			strings.Compare(a.Env, b.Env),
			strings.Compare(a.Repo, b.Repo),
		)
	})
	return states, truncated, nil
}

// newerRun orders two runs on the same lane by queue time, falling back to the
// id so the choice is stable when two records share a timestamp. Queue time is
// the only stamp a still-pending run has, which is why it and not EndedAt
// orders "the newest run on this lane".
func newerRun(a, b Run) bool {
	if a.QueuedAt != b.QueuedAt {
		return a.QueuedAt > b.QueuedAt
	}
	return a.ID > b.ID
}

// finishedLater orders two finished runs by when they landed. Only ever asked
// about successes, which all have an EndedAt; queue time and the id break ties.
//
// Deliberately not newerRun: on a lane where two runs queued together and
// finished out of order, the one that finished last is what is deployed, and
// the earlier-finishing one is the record the engine stamped as superseded.
func finishedLater(a, b Run) bool {
	if a.EndedAt != b.EndedAt {
		return a.EndedAt > b.EndedAt
	}
	return newerRun(a, b)
}
