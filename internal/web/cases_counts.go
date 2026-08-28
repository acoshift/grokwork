package web

import "github.com/moonrhythm/hime"

// caseCounts is the JSON body for GET /partials/cases/counts.
// Zeros are included so JS can clear a stage or filter label that just emptied.
type caseCounts struct {
	Intake      int `json:"intake"`
	Investigate int `json:"investigate"`
	Answered    int `json:"answered"`
	Fixing      int `json:"fixing"`
	Shipping    int `json:"shipping"`
	Closed      int `json:"closed"`
	Mine        int `json:"mine"`
	Unassigned  int `json:"unassigned"`
	Breached    int `json:"breached"`
}

func (s *Server) partialCasesCounts(ctx *hime.Context) error {
	d, err := s.casesPartialData(ctx)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	b := d.Cases
	return ctx.NoCache().JSON(caseCounts{
		Intake:      b.Intake,
		Investigate: b.Investigate,
		Answered:    b.Answered,
		Fixing:      b.Fixing,
		Shipping:    b.Shipping,
		Closed:      b.Closed,
		Mine:        b.Mine,
		Unassigned:  b.Unassigned,
		Breached:    b.Breached,
	})
}
