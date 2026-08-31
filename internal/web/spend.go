package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/spend"
)

// Spend reporting: what the agent runs cost.
//
// Two pages over one rollup — the global lead view (/spend) and the workspace one
// (/projects/{p}/spend) — because "which project is burning money" and "who in
// this project is burning it" are different questions with the same arithmetic
// behind them.
//
// Visibility is enforced by handing spend.Build a per-project gate rather than by
// filtering rows afterwards. A rollup is a cross-project read: the actor and
// session tables would otherwise carry another project's spend even when its own
// row is hidden, and a member could read a project's burn rate by looking at who
// works on it.

// spendVisibleFunc is the per-project gate for a rollup, or nil when the viewer
// may see everything (auth off, or an admin).
func (s *Server) spendVisibleFunc(ctx *hime.Context) func(string) bool {
	allowed := s.visibleProjectSet(ctx)
	if allowed == nil {
		return nil
	}
	return func(project string) bool {
		_, ok := allowed[project]
		return ok
	}
}

// spendReport builds the rollup for a query, applying the viewer's visibility.
// The project filter is applied on top of the gate, never instead of it.
func (s *Server) spendReport(ctx *hime.Context, project, actorID string) (spend.Report, error) {
	if s.history == nil {
		return spend.Report{}, nil
	}
	return spend.Build(s.history, s.cfg, spend.Query{
		Project: strings.TrimSpace(project),
		ActorID: strings.TrimSpace(actorID),
		Visible: s.spendVisibleFunc(ctx),
	})
}

// spendPage is the cross-project cost report.
func (s *Server) spendPage(ctx *hime.Context) error {
	actor := strings.TrimSpace(ctx.FormValue("actor"))
	rep, err := s.spendReport(ctx, "", actor)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("spend rollup: " + err.Error())
	}
	d := s.basePage(ctx)
	d.Title = "Spend"
	d.IsSpend = true
	d.Spend = rep
	d.SpendActor = actor
	d.RatesConfigured = s.cfg.Snapshot().ModelRatesSet
	return s.viewPage(ctx, "spend", d)
}

// spendScoped is the workspace cost report: one project, broken down by actor and
// by session.
func (s *Server) spendScoped(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	actor := strings.TrimSpace(ctx.FormValue("actor"))
	rep, err := s.spendReport(ctx, project, actor)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("spend rollup: " + err.Error())
	}
	d := s.basePage(ctx)
	d.Title = project + " · Spend"
	d.IsSpend = true
	d.Project = project
	d.Spend = rep
	d.SpendActor = actor
	d.RatesConfigured = s.cfg.Snapshot().ModelRatesSet
	return s.viewPage(ctx, "spend", d)
}

// --- rate table (admin config) ------------------------------------------------

// updateModelRates persists the per-model price table.
//
// The form posts one row per model as model[i] + the four rate fields, and the
// whole table is replaced: a merge would leave no way to clear a rate, and
// clearing one is how an operator says "stop reporting dollars for this model"
// after a price change they have not looked up yet.
func (s *Server) updateModelRates(ctx *hime.Context) error {
	rates, n, err := parseModelRateForm(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "modelRates"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.rates", "", err)
	}
	if err := s.cfg.SetModelRates(rates); err != nil {
		return s.configPageRedirect(ctx, "config.rates", "", err)
	}
	msg := "Cleared every model rate — reports will show tokens only"
	if n > 0 {
		msg = fmt.Sprintf("Saved rates for %d model(s)", n)
	}
	return s.configPageRedirect(ctx, "config.rates", msg, nil)
}

// fillModelRatesFromOfficial copies list prices from each agent's official
// docs (xAI, Anthropic, Cursor) onto matched rows. Matched figures are
// replaced so a price change can be refreshed; unmatched custom names stay.
func (s *Server) fillModelRatesFromOfficial(ctx *hime.Context) error {
	cat, err := config.FetchOfficialRates(ctx.Request.Context(), s.officialRateHTTP, s.officialRateURLs)
	if err != nil {
		s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "modelRates.official"})
		return s.configPageRedirect(ctx, "config.rates", "", err)
	}
	res, err := s.cfg.ApplyOfficialRates(cat)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{
		"section":   "modelRates.official",
		"updated":   len(res.Updated),
		"unmatched": len(res.Unmatched),
		"sources":   res.Sources,
	})
	if err != nil {
		return s.configPageRedirect(ctx, "config.rates", "", err)
	}
	msg := "Official docs had no match for the models in the table"
	if len(res.Updated) > 0 {
		src := strings.Join(res.Sources, ", ")
		if src == "" {
			src = "official docs"
		}
		msg = fmt.Sprintf("Updated rates for %d model(s) from %s", len(res.Updated), src)
	}
	if len(res.Errors) > 0 && len(res.Updated) > 0 {
		msg += " · " + res.Errors[0]
	}
	return s.configPageRedirect(ctx, "config.rates", msg, nil)
}

// parseModelRateForm reads the rate table out of the form. A bad figure fails the
// whole save rather than being dropped: a silently ignored typo looks exactly like
// "not configured", which turns dollars off without telling anyone.
func parseModelRateForm(ctx *hime.Context) (map[string]config.ModelRate, int, error) {
	if err := ctx.Request.ParseForm(); err != nil {
		return nil, 0, err
	}
	out := map[string]config.ModelRate{}
	set := 0
	for i, name := range ctx.Request.PostForm["model"] {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		var r config.ModelRate
		for _, f := range []struct {
			field string
			dst   **float64
		}{
			{"input", &r.InputPerMTok},
			{"output", &r.OutputPerMTok},
			{"cacheRead", &r.CacheReadPerMTok},
			{"cacheWrite", &r.CacheWritePerMTok},
		} {
			v, err := config.ParseRate(formValueAt(ctx, f.field, i))
			if err != nil {
				return nil, 0, fmt.Errorf("%s %s: %w", name, f.field, err)
			}
			*f.dst = v
		}
		if r.IsEmpty() {
			continue
		}
		out[name] = r
		set++
	}
	return out, set, nil
}

// formValueAt reads the i-th value of a repeated form field. Rows are matched by
// position, which is why every row posts all five fields (a blank rate still
// posts an empty string) — a sparse form would silently shift rates onto the
// wrong models.
func formValueAt(ctx *hime.Context, field string, i int) string {
	vs := ctx.Request.PostForm[field]
	if i >= len(vs) {
		return ""
	}
	return vs[i]
}

// --- formatting ---------------------------------------------------------------

// formatTokens renders a token count compactly. Exact numbers are useless at this
// scale (nobody reads 16,502,391) and the magnitude is the whole signal. Three
// significant figures throughout, so "510k" does not render as "510.0k".
func formatTokens(n int) string {
	if n <= 0 {
		return "0"
	}
	scale := func(div float64, unit string) string {
		v := float64(n) / div
		if v >= 100 {
			return fmt.Sprintf("%.0f%s", v, unit)
		}
		return fmt.Sprintf("%.1f%s", v, unit)
	}
	switch {
	case n >= 1_000_000_000:
		return scale(1e9, "B")
	case n >= 1_000_000:
		return scale(1e6, "M")
	case n >= 1_000:
		return scale(1e3, "k")
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatUSD renders dollars with enough precision to be meaningful at both ends
// of the range: a single cheap turn is fractions of a cent, a month of a team's
// work is thousands of dollars, and rounding either to two decimals hides one of
// them.
func formatUSD(v float64) string {
	switch {
	case v <= 0:
		return "$0.00"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	case v < 1000:
		return fmt.Sprintf("$%.2f", v)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// spendCost renders a row's dollar figure, or an honest blank when nothing in it
// could be priced. Never "$0.00" for an unpriced row — that reads as a measured
// cost of nothing, and this is the one number an operator would act on.
func spendCost(r spend.Row) string {
	if !r.HasCost() {
		return "—"
	}
	out := formatUSD(r.Dollars)
	if r.Partial() {
		// A floor, not a total: some turns in this row have no rate.
		out = "≥ " + out
	}
	return out
}

// spendModels labels the models behind a row, naming the unstamped ones rather
// than dropping them — those are exactly the rows with no cost.
func spendModels(r spend.Row) string {
	if len(r.Models) == 0 {
		return "—"
	}
	out := make([]string, 0, len(r.Models))
	for _, m := range r.Models {
		if m == "" {
			m = "unnamed"
		}
		out = append(out, m)
	}
	return strings.Join(out, ", ")
}
