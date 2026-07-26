// Package spend aggregates what agent runs cost.
//
// The inputs are the per-turn token records in internal/history and the per-model
// rate table in internal/config; the output is a rollup by project, by actor, and
// by session. Nothing here talks to a CLI or a store directly — it is a pure fold
// over turns, so the interesting cases (a model with no rate, a project the viewer
// may not see) are testable without a filesystem.
//
// Two rules run through the whole package:
//
//   - Tokens and dollars are separate answers. A turn always contributes its
//     tokens; it contributes dollars only if its model has a rate covering every
//     class it used. A report therefore says "1.2M tokens, $4.10, 3 turns
//     unpriced" rather than quietly under-reporting the total.
//   - Visibility is applied per turn, not per report. A rollup that spans projects
//     is a cross-project read, so a member must never see a project's spend
//     through the actor or session totals either.
package spend

import (
	"cmp"
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
)

// Pricer turns a model's tokens into dollars. *config.Config satisfies it; a nil
// Pricer prices nothing, which is the correct behaviour for a deployment that has
// configured no rates.
type Pricer interface {
	PriceTokens(model string, t config.TokenCounts) (float64, bool)
}

// ThreadSource walks every recorded turn log. *history.Store satisfies it.
type ThreadSource interface {
	Walk(func(history.Thread) error) error
}

// Query selects and scopes a report.
type Query struct {
	// Project limits the report to one project. Empty covers every visible one.
	Project string
	// ActorID limits the report to one actor's turns.
	ActorID string
	// Visible reports whether the viewer may see a project's spend. Nil is
	// unrestricted (auth off, or an admin). When set it is authoritative: a turn
	// whose project cannot be determined is dropped rather than counted, since an
	// unattributable turn must not become a hole in the ACL.
	Visible func(project string) bool
}

// Row is one aggregated bucket: a project, an actor, a session, or the total.
type Row struct {
	// Key identifies the bucket (project name, actor id, thread id). Empty on the
	// report total.
	Key string
	// Label is the display name (actor display name, session goal or last prompt).
	Label string
	// Project is the owning project, for rows that link somewhere project-scoped.
	Project string
	Turns   int
	Tokens  config.TokenCounts
	// Dollars is the cost of the priced turns only. Read it together with
	// Unpriced: a figure with unpriced turns behind it is a floor, not a total.
	Dollars  float64
	Priced   int
	Unpriced int
	// Models are the distinct model names seen, sorted. An unnamed model appears
	// as "" and renders as "unknown" — those are the pre-stamp sessions.
	Models []string
	// LastAt is the newest turn timestamp in the bucket (RFC3339, as recorded).
	LastAt string
}

// TotalTokens is every billable token in the bucket.
func (r Row) TotalTokens() int { return r.Tokens.Total() }

// HasCost reports whether any dollar figure could be computed at all.
func (r Row) HasCost() bool { return r.Priced > 0 }

// Partial reports a dollar figure that is known to be incomplete, which the UI
// must say out loud — an operator comparing two projects has no other way to know
// one of them is missing a rate.
func (r Row) Partial() bool { return r.Priced > 0 && r.Unpriced > 0 }

// Report is the full rollup.
type Report struct {
	Total     Row
	ByProject []Row
	ByActor   []Row
	BySession []Row
	// UnpricedModels names the models that carried tokens but produced no dollars,
	// sorted. This is the actionable half of "no cost shown": it turns a mystery
	// into a list of rows to fill in on the rate table.
	UnpricedModels []string
}

// Build folds every visible turn into a report.
func Build(src ThreadSource, p Pricer, q Query) (Report, error) {
	var out Report
	if src == nil {
		return out, nil
	}
	projects := map[string]*Row{}
	actors := map[string]*Row{}
	sessions := map[string]*Row{}
	unpriced := map[string]bool{}
	wantActor := strings.TrimSpace(q.ActorID)

	err := src.Walk(func(th history.Thread) error {
		for _, turn := range th.Turns {
			project := strings.TrimSpace(turn.Project)
			if project == "" {
				project = strings.TrimSpace(th.Project)
			}
			if q.Project != "" && project != q.Project {
				continue
			}
			if q.Visible != nil && !q.Visible(project) {
				continue
			}
			actorKey, actorLabel := turnActor(turn)
			if wantActor != "" && actorKey != wantActor {
				continue
			}
			tokens := turnTokens(turn)
			if tokens.IsZero() {
				// No token record: an older turn, or a run whose CLI reported
				// nothing. Counting it as a zero-cost turn would dilute every
				// average on the page, so it is not part of the report at all.
				continue
			}
			dollars, ok := price(p, turn.Model, tokens)
			if !ok {
				unpriced[strings.TrimSpace(turn.Model)] = true
			}

			out.Total.add(turn, tokens, dollars, ok)
			bucket(projects, project).addLabeled(turn, tokens, dollars, ok, project, project)
			bucket(actors, actorKey).addLabeled(turn, tokens, dollars, ok, actorLabel, project)
			bucket(sessions, th.ThreadID).addLabeled(turn, tokens, dollars, ok, sessionLabel(turn), project)
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}

	out.ByProject = sortedRows(projects)
	out.ByActor = sortedRows(actors)
	out.BySession = sortedRows(sessions)
	for name := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, name)
	}
	slices.Sort(out.UnpricedModels)
	return out, nil
}

// ForThread is the single-session rollup the session page shows. It takes an
// already-loaded thread rather than a source: the page has one in hand, and
// walking every log to find it again would make opening a session O(all history).
func ForThread(th history.Thread, p Pricer) Row {
	row := Row{Key: th.ThreadID, Project: strings.TrimSpace(th.Project)}
	for _, turn := range th.Turns {
		tokens := turnTokens(turn)
		if tokens.IsZero() {
			continue
		}
		dollars, ok := price(p, turn.Model, tokens)
		row.add(turn, tokens, dollars, ok)
	}
	return row
}

// price is Pricer.PriceTokens with a nil-safe receiver.
func price(p Pricer, model string, t config.TokenCounts) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return p.PriceTokens(model, t)
}

// turnTokens is the billable classes of one turn. Occupancy is deliberately not
// read here: it is a point-in-time measurement of the context window, and summing
// it across turns would produce a number that means nothing.
func turnTokens(turn history.Turn) config.TokenCounts {
	u := turn.Usage
	if u == nil {
		return config.TokenCounts{}
	}
	t := config.TokenCounts{
		Input:         u.InputTokens,
		CacheRead:     u.CacheReadTokens,
		CacheCreation: u.CacheCreationTokens,
		Output:        u.OutputTokens,
	}
	if t.IsZero() && u.TotalTokens > 0 {
		// A CLI that reported only a total: count the tokens under Input so the
		// rollup still shows them. It cannot be priced correctly (the classes carry
		// different rates), and PriceTokens will fail on it unless an input rate
		// exists — which is the honest outcome either way.
		t.Input = u.TotalTokens
	}
	return t
}

// turnActor identifies who ran a turn. The id is the key because display names
// change; the name is only a label.
func turnActor(turn history.Turn) (key, label string) {
	id := strings.TrimSpace(turn.UserID)
	name := strings.TrimSpace(turn.User)
	switch {
	case id != "":
		if name == "" {
			name = id
		}
		return id, name
	case name != "":
		return name, name
	default:
		return "", "unknown"
	}
}

func sessionLabel(turn history.Turn) string {
	if s := strings.TrimSpace(turn.Prompt); s != "" {
		return truncate(s, 80)
	}
	return ""
}

func bucket(m map[string]*Row, key string) *Row {
	r, ok := m[key]
	if !ok {
		r = &Row{Key: key}
		m[key] = r
	}
	return r
}

func (r *Row) addLabeled(turn history.Turn, tokens config.TokenCounts, dollars float64, priced bool, label, project string) {
	if r.Label == "" {
		r.Label = label
	}
	if r.Project == "" {
		r.Project = project
	}
	r.add(turn, tokens, dollars, priced)
}

func (r *Row) add(turn history.Turn, tokens config.TokenCounts, dollars float64, priced bool) {
	r.Turns++
	r.Tokens.Add(tokens)
	if priced {
		r.Dollars += dollars
		r.Priced++
	} else {
		r.Unpriced++
	}
	model := strings.TrimSpace(turn.Model)
	if !slices.Contains(r.Models, model) {
		r.Models = append(r.Models, model)
		slices.Sort(r.Models)
	}
	if turn.At > r.LastAt {
		r.LastAt = turn.At
	}
}

// sortedRows orders buckets by cost, then tokens, then key. Cost first because
// the question the page answers is "what is expensive"; the key tiebreak keeps
// the order stable for tests and for a viewer comparing two loads.
func sortedRows(m map[string]*Row) []Row {
	out := make([]Row, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int {
		if c := cmp.Compare(b.Dollars, a.Dollars); c != 0 {
			return c
		}
		if c := cmp.Compare(b.TotalTokens(), a.TotalTokens()); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}
