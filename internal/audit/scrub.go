package audit

import "regexp"

// pathChar is one character of a path: everything up to whitespace or a quote
// form, since those are what a message wraps a path in.
const pathChar = "[^\\s\"'`)]"

// pathRE matches absolute filesystem paths and managed worktree paths.
//
// Deliberately not anchored on a leading delimiter: the text this guards is
// mostly git/gh stderr, where a path arrives glued to a colon —
// "fatal: not a git repository:/Users/…".
//
// The leading directory is an allowlist rather than "any absolute path" on
// purpose: audit details legitimately carry URLs (PR and issue links are the
// most useful field in the log), and a blanket `/\S+` would shred every one of
// them. The trade is that a host keeping checkouts somewhere exotic is not
// covered by the static list alone — RedactRoots exists for that.
var pathRE = regexp.MustCompile(`(?i)(?:` +
	`[A-Za-z]:\\` + pathChar + `+` + // C:\Users\…
	`|/(?:Users|home|var|tmp|private|opt|usr|srv|mnt|data|workspace|root|Volumes|Applications|Library|etc)/` + pathChar + `*` +
	`|data/worktrees/` + pathChar + `*` +
	`|data/cherrypick/` + pathChar + `*)`)

// ScrubPaths removes filesystem paths from text bound for the audit log.
//
// Exported because both surfaces need the same answer: the audit file is read
// by one person asking one question, and a path that is redacted when Discord
// writes the row but present when the web UI writes the identical row makes the
// log's guarantee meaningless. Append applies it centrally, so call this
// directly only when scrubbing something before it reaches an Event.
func ScrubPaths(s string) string {
	if s == "" {
		return s
	}
	return pathRE.ReplaceAllString(s, "[path]")
}

// scrubDetail returns a copy of detail with every string and []string value
// scrubbed. The map is copied rather than mutated: callers build detail maps
// inline and sometimes reuse them, and an audit write must not edit a caller's
// data as a side effect.
//
// Only strings are walked. Numbers and bools cannot carry a path, and a nested
// map[string]any is not produced by any current call site — if one appears, it
// is better that it goes unscrubbed and visibly so than that this function
// silently deep-copies structures it does not understand.
func scrubDetail(detail map[string]any) map[string]any {
	if len(detail) == 0 {
		return detail
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		switch t := v.(type) {
		case string:
			out[k] = ScrubPaths(t)
		case []string:
			cp := make([]string, len(t))
			for i, s := range t {
				cp[i] = ScrubPaths(s)
			}
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}
