package web

import (
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"
)

// Provenance back-links.
//
// A detail page is reachable from several boards, so its "←" crumb cannot be
// derived from the record alone: a case opened from the case board should
// return to that board — with the filters the user had applied — not to the
// sessions list. The linking board therefore stamps where it came from as a
// ?back= query param carrying a root-relative URL.
//
// The param is attacker-controllable (it lands in an href), so it is never
// echoed back as-is. resolveBackLink re-derives the label from an allowlist of
// known board paths and rejects anything else, which makes an open redirect or
// a javascript: crumb impossible without a second check at render time.

// backLinkSections maps a board path suffix to the crumb label it renders.
// Both the global board (/cases) and the workspace board
// (/projects/{name}/cases) resolve through the same entry.
var backLinkSections = map[string]string{
	"cases":     "Cases",
	"sessions":  "Sessions",
	"ship":      "Ship",
	"reviews":   "Reviews",
	"issues":    "Issues",
	"worktrees": "Worktrees",
	"commits":   "Commits",
	"deploys":   "Deploys",
	// Search is a board like the others for crumb purposes: its query string is
	// the filter state, and a case reached by pasting its key should return to
	// the results rather than to the sessions list.
	"search": "Search",
}

// resolveBackLink validates a ?back= value and returns the href to render plus
// its crumb label. ok is false for anything that is not a known board path, in
// which case the caller keeps its own default crumb.
//
// Only the path is matched; the query string rides along untouched so board
// filters survive the round trip. It cannot smuggle a host: a value that parses
// with a scheme or an authority — or that a browser would re-read as one, like
// a backslash — is rejected before the path is looked at.
func resolveBackLink(raw string) (href, label string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "", "", false
	}
	// Browsers normalise "\" to "/", so "/\evil.example" would leave this
	// function as a path and arrive at the browser as a protocol-relative URL.
	if strings.ContainsAny(raw, "\\\x00") {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "", "", false
	}
	// Match on the DECODED path. url.Parse turns "/%2fcases" into "//cases" and
	// "/.%2e/config" into "/../config", so a check against the raw string alone
	// lets an off-origin or traversing target through the front door and out
	// again as a real one.
	label, ok = backLinkLabel(u.Path)
	if !ok {
		return "", "", false
	}
	// Emit the escaped form: a project whose name needs encoding must keep it,
	// and what reaches the template has then been through the parser rather
	// than merely past it.
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, label, true
}

// backLinkLabel names the board a path points at: "/cases" globally,
// "/projects/{name}/cases" inside a workspace. The issue detail page is a
// board for crumb purposes too — a session opened from an issue hub should
// return to that issue (`/projects/{name}/issues/{n}`). Anything else is
// unknown.
//
// Segments are checked individually rather than after trimming slashes: an
// empty one means a doubled separator (a protocol-relative URL once it reaches
// the browser), and "." / ".." mean the path does not denote what it appears
// to. Trimming would quietly turn "//cases" into "cases" and accept it.
func backLinkLabel(path string) (string, bool) {
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	for _, p := range parts[1:] {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	switch {
	case len(parts) == 2:
		label, ok := backLinkSections[parts[1]]
		return label, ok
	case len(parts) == 4 && parts[1] == "projects":
		label, ok := backLinkSections[parts[3]]
		return label, ok
	case len(parts) == 5 && parts[1] == "projects" && parts[3] == "issues" && allDigits(parts[4]):
		return "Issue", true
	// PR detail: /prs/{owner}/{repo}/{n} — session crumbs opened from the PR
	// Sessions list return here rather than to the sessions board.
	case len(parts) == 5 && parts[1] == "prs" && allDigits(parts[4]):
		return "PR", true
	}
	return "", false
}

// allDigits reports whether s is non-empty and every byte is an ASCII digit.
// Used to keep the issue-detail crumb allowlist tight (reject /issues/42abc).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return r < '0' || r > '9'
	}) < 0
}

// caseBoardURL is the board a case unit belongs to — the workspace board when
// the project is known, the cross-project board otherwise.
func caseBoardURL(project string) string {
	if project == "" {
		return "/cases"
	}
	return "/projects/" + project + "/cases"
}

// sessionBackLink picks the session page's "←" crumb, in falling precedence:
//
//  1. a valid ?back= stamped by the board the user came from — the only form
//     that can restore that board's filters;
//  2. the case board, for a case unit. A case's home is the board, and this is
//     what survives the POST round trips its own rail actions make: escalate /
//     close / reopen redirect back here without the query param, so a
//     provenance-only crumb would silently degrade to Sessions mid-workflow;
//  3. the sessions list — the default for ordinary work units.
func (s *Server) sessionBackLink(ctx *hime.Context, d pageData) (href, label string) {
	if href, label, ok := resolveBackLink(ctx.FormValue("back")); ok {
		return href, label
	}
	if d.SessionEntry.IsCase() {
		return caseBoardURL(d.Project), "Cases"
	}
	if d.Project != "" {
		return "/projects/" + d.Project + "/sessions", "Sessions"
	}
	return "/sessions", "Sessions"
}
