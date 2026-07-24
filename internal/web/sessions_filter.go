package web

import (
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// sessionFilters is the sessions list filter state, parsed from query params.
// Zero value = unfiltered, which preserves the historical "show everything"
// behavior of both the global hub and the workspace page.
type sessionFilters struct {
	State    string   // "", "active", "closed" (case closed), or a canonical label
	Query    string   // free-text over thread id / prompt / user / project
	Project  string   // global hub only ("" = all projects; workspace fixes it via path)
	Projects []string // dropdown options on the global hub
	Total    int      // row count before filtering (for "x of y" chrome)
}

// Filtered reports whether any filter narrows the list.
func (f sessionFilters) Filtered() bool {
	return f.State != "" || f.Query != "" || f.Project != ""
}

// parseSessionFilters reads the sessions list query params. withProject is
// true on the global hub, where ?project= is a data filter like /ship (the
// shell stays global — see navScopeFromURL). Unknown state values fall back
// to All so stale URLs never 4xx or silently mismatch.
func parseSessionFilters(ctx *hime.Context, withProject bool) sessionFilters {
	f := sessionFilters{
		State: strings.TrimSpace(ctx.FormValue("state")),
		Query: strings.TrimSpace(ctx.FormValue("q")),
	}
	if withProject {
		f.Project = strings.TrimSpace(ctx.FormValue("project"))
	}
	switch f.State {
	case "active", "closed":
		// Meta states; "closed" must not reach ParseLabel (alias of abandoned).
	default:
		if lab, ok := sessionstore.ParseLabel(f.State); ok {
			f.State = lab
		} else {
			f.State = ""
		}
	}
	return f
}

// filterSessionRows applies f to merged, visibility-filtered session rows.
func filterSessionRows(threads []history.Summary, f sessionFilters) []history.Summary {
	if !f.Filtered() {
		return threads
	}
	q := strings.ToLower(f.Query)
	out := make([]history.Summary, 0, len(threads))
	for _, t := range threads {
		if f.Project != "" && t.Project != f.Project {
			continue
		}
		if !sessionStateMatches(t, f.State) {
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
// no label and count as open. Closed cases freeze their label (K18) and the
// list displays "closed" instead, so they match only "closed" (and All).
func sessionStateMatches(t history.Summary, state string) bool {
	if state == "" {
		return true
	}
	closedCase := t.Mode == "case" && t.Phase == "closed"
	if state == "closed" {
		return closedCase
	}
	if closedCase {
		return false
	}
	label := t.Label
	if label == "" {
		label = sessionstore.LabelOpen
	}
	if state == "active" {
		return label != sessionstore.LabelDone && label != sessionstore.LabelAbandoned
	}
	return label == state
}

// sessionQueryMatches is a case-insensitive substring match over the fields a
// user sees on a list row. q must already be lowercased.
func sessionQueryMatches(t history.Summary, q string) bool {
	for _, s := range []string{t.ThreadID, t.LastPrompt, t.LastUser, t.Project} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}
