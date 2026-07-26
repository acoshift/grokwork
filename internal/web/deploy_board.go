package web

import (
	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/deploy"
)

// deployBoardRow is one lane on the cross-project deploy board: what is live on
// one project's service+environment, and whether anything has happened since.
type deployBoardRow struct {
	Project string
	Repo    string
	Service string
	Env     string

	// The live deploy. HasCurrent is false for a lane whose runs all failed —
	// the row still renders, because "nothing has ever shipped to prod" is the
	// most important thing this board can say about a lane.
	// A current run is a succeeded one by construction, so the row carries no
	// status of its own — the Status column says "live" or "never deployed",
	// and only the newest run (below) can add a word to that.
	HasCurrent bool
	RunID      string
	ShortSHA   string
	Subject    string
	Actor      string
	Age        string
	At         string

	// The newest run when it is not the live one: a deploy in flight, or a
	// failed attempt sitting on top of a good deploy. Both mean the SHA in the
	// row is still what is running, and both are why an operator opened this
	// page.
	LatestRunID  string
	LatestStatus string
	LatestAge    string
}

// NeedsAttention marks a lane nobody should walk away from: one that has never
// had a successful deploy, or whose newest run failed, was cancelled, or was
// interrupted by a restart — as opposed to one that is merely mid-deploy.
func (r deployBoardRow) NeedsAttention() bool {
	switch deploy.Status(r.LatestStatus) {
	case deploy.StatusFailed, deploy.StatusCancelled, deploy.StatusInterrupted:
		return true
	}
	return !r.HasCurrent
}

// deployBoard backs the global /deploys lead view.
type deployBoard struct {
	Rows     []deployBoardRow
	Projects int
	Live     int
	// Attention counts lanes with no successful deploy or a failed newest run.
	Attention int
	// Truncated says the fold stopped at ScanLimit records, so a lane that has
	// not been touched in a long time may be missing from Rows entirely. Said
	// out loud on the page: silently omitting a lane reads as "no such
	// environment", which is a lie the board must not tell.
	Truncated bool
	ScanLimit int
}

// buildDeployBoard folds the run store into one row per visible lane.
//
// The project allowlist is ProjectsVisibleTo (all configured projects for an
// admin, the viewer's own otherwise) and it is applied as an allowlist, not a
// denylist: a run whose project is unknown to the config — a project that was
// removed, leaving its history behind — renders for nobody. This page
// aggregates every project at once, so a missed filter here leaks not just
// deploy activity but the existence and naming of other teams' projects.
func (s *Server) buildDeployBoard(ctx *hime.Context) (deployBoard, error) {
	limit := s.deployScanLimit
	if limit <= 0 {
		limit = deploy.DefaultLaneScanLimit
	}
	board := deployBoard{ScanLimit: limit}
	if s.deploys == nil {
		return board, nil
	}
	states, truncated, err := s.deploys.Store().LaneStates(limit)
	if err != nil {
		return board, err
	}
	board.Truncated = truncated

	allowed := make(map[string]struct{})
	for _, n := range s.filterProjectNames(ctx) {
		allowed[n] = struct{}{}
	}
	seen := make(map[string]struct{}, len(allowed))
	for _, st := range states {
		if _, ok := allowed[st.Project]; !ok {
			continue
		}
		row := deployBoardRow{
			Project: st.Project,
			Repo:    st.Repo,
			Service: st.Service,
			Env:     st.Env,
		}
		if st.HasCurrent {
			cur := st.Current
			row.HasCurrent = true
			row.RunID = cur.ID
			row.ShortSHA = cur.ShortSHA
			row.Subject = cur.Subject
			row.Actor = cur.ActorName
			if row.Actor == "" {
				row.Actor = cur.ActorID
			}
			row.At = deployRunStamp(cur)
			row.Age = relativeAge(row.At)
		}
		if st.Unsettled() {
			row.LatestRunID = st.Latest.ID
			row.LatestStatus = string(st.Latest.Status)
			row.LatestAge = relativeAge(deployRunStamp(st.Latest))
		}
		if row.HasCurrent {
			board.Live++
		}
		if row.NeedsAttention() {
			board.Attention++
		}
		seen[st.Project] = struct{}{}
		board.Rows = append(board.Rows, row)
	}
	board.Projects = len(seen)
	return board, nil
}

// deployRunStamp is the run's most meaningful timestamp: when it ended, else
// when it started, else when it was queued. A run still waiting on a busy lane
// has only the last of the three.
func deployRunStamp(r deploy.Run) string {
	if r.EndedAt != "" {
		return r.EndedAt
	}
	if r.StartedAt != "" {
		return r.StartedAt
	}
	return r.QueuedAt
}

// deploysBoard is the cross-project "what is running where" lead view.
//
// Global, like /ship and /worktrees: the per-project page at
// /projects/{p}/deploys answers "what would a deploy of this project run",
// which is a different question and needs the manifest read from git. This one
// reads only the run store, so it costs the same whether an operator has one
// service or forty.
func (s *Server) deploysBoard(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Deploys"
	d.IsDeploys = true
	board, err := s.buildDeployBoard(ctx)
	if err != nil {
		d.Error = err.Error()
	}
	d.DeployBoard = board
	return s.viewPage(ctx, "deploys_board", d)
}

// partialDeploysBoard re-renders the board on the deploy SSE domain.
func (s *Server) partialDeploysBoard(ctx *hime.Context) error {
	d := s.basePage(ctx)
	board, err := s.buildDeployBoard(ctx)
	if err != nil {
		d.Error = err.Error()
	}
	d.DeployBoard = board
	return s.viewFragment(ctx, "deploys_board", "deploys_board_table", d)
}
