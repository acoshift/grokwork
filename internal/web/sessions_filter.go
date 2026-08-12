package web

import (
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// activeRecency keeps freshly finished work on the default Active view:
// settled rows still match "active" for this long after their last update,
// then drop off. Settled = terminal label, closed case, non-case units with
// all tracked PRs terminal, or history-only (no session overlay).
const activeRecency = 24 * time.Hour

// sessionFilters is the sessions list filter state, parsed from query params.
type sessionFilters struct {
	State    string   // "active" (default), "all", "closed", "running" (live agent job), or a canonical label
	Query    string   // free-text over thread id / goal / last prompt / user / project
	Project  string   // global hub only ("" = all projects; workspace fixes it via path)
	Projects []string // dropdown options on the global hub
	Total    int      // row count before filtering (for "x of y" chrome)
}

// Filtered reports whether f narrows the list at all.
func (f sessionFilters) Filtered() bool {
	return f.State != "all" || f.Query != "" || f.Project != ""
}

// parseSessionFilters reads the sessions list query params. withProject is
// true on the global hub, where ?project= is a data filter like /ship (the
// shell stays global — see navScopeFromURL). No state (and unknown state
// values, e.g. stale URLs) means the default Active view.
func parseSessionFilters(ctx *hime.Context, withProject bool) sessionFilters {
	f := sessionFilters{
		Query: strings.TrimSpace(ctx.FormValue("q")),
	}
	if withProject {
		f.Project = strings.TrimSpace(ctx.FormValue("project"))
	}
	state := strings.TrimSpace(ctx.FormValue("state"))
	switch state {
	case "all", "active", "closed", "running":
		// Meta states; "closed" must not reach ParseLabel (alias of abandoned).
		// "running" is the live agent job overlay, not a lifecycle label.
	default:
		if lab, ok := sessionstore.ParseLabel(state); ok {
			state = lab
		} else {
			state = "active"
		}
	}
	f.State = state
	return f
}

// filterSessionRows applies f to merged, visibility-filtered session rows.
func filterSessionRows(threads []history.Summary, f sessionFilters, now time.Time) []history.Summary {
	if !f.Filtered() {
		return threads
	}
	q := strings.ToLower(f.Query)
	out := make([]history.Summary, 0, len(threads))
	for _, t := range threads {
		if f.Project != "" && t.Project != f.Project {
			continue
		}
		if !sessionStateMatches(t, f.State, now) {
			continue
		}
		if q != "" && !sessionQueryMatches(t, q) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// sessionStateMatches matches a row against the state filter. Rows carry the
// effective lifecycle label from the session overlay; history-only rows have
// no label and count as open for explicit label filters. "active" is work
// still in flight (or a live agent job) plus settled rows updated within
// activeRecency, so a just-shipped session does not vanish from the default
// view. Settled covers terminal labels, closed cases, units whose tracked PRs
// are all merged/closed (label stays needs_review until a human marks done —
// but the work is not active), and history-only rows (session tombstone
// pruned; no live unit). Closed cases freeze their label (K18) and the list
// displays "closed" instead, so label filters skip them and only "closed"
// (or a recent "active" / "all") matches.
func sessionStateMatches(t history.Summary, state string, now time.Time) bool {
	if state == "all" {
		return true
	}
	closedCase := t.Mode == "case" && t.Phase == "closed"
	if state == "closed" {
		return closedCase
	}
	// Empty Label means no session overlay ran (history-only). Overlay always
	// writes EffectiveLabel, so this is never an empty stored "open".
	historyOnly := t.Label == ""
	label := t.Label
	if historyOnly {
		label = sessionstore.LabelOpen
	}
	if state == "active" {
		// Live agent work is active regardless of label / PR terminal state.
		if t.Running {
			return true
		}
		// AllPRsTerminal settles eng units whose PRs finished but the label was
		// never marked done. Open cases keep their own phase lifecycle — a
		// merged PR does not mean the customer thread is finished.
		shipped := t.AllPRsTerminal && t.Mode != "case"
		settled := historyOnly || closedCase ||
			label == sessionstore.LabelDone ||
			label == sessionstore.LabelAbandoned ||
			shipped
		return !settled || updatedWithin(t.UpdatedAt, now, activeRecency)
	}
	if state == "running" {
		// Live agent job only — not the lifecycle label "in_progress", which
		// stays after a run ends until a human closes the unit.
		return t.Running
	}
	if closedCase {
		return false
	}
	return label == state
}

// updatedWithin reports whether an RFC3339 timestamp falls inside the last d.
// Unparseable or empty timestamps do not qualify.
func updatedWithin(rfc3339 string, now time.Time, d time.Duration) bool {
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return false
	}
	return now.Sub(ts) <= d
}

// sessionQueryMatches is a case-insensitive substring match over the fields a
// user sees on a list row (plus last prompt for recency search). q must already
// be lowercased.
func sessionQueryMatches(t history.Summary, q string) bool {
	for _, s := range []string{t.ThreadID, t.Goal, t.LastPrompt, t.LastUser, t.Project} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}
