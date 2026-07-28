package ghpr

import (
	"strconv"
	"strings"
)

// RenderedLine is one diff line with resolved line numbers for display.
// Old/New are 0 when the side has no number (adds, deletes, meta lines).
type RenderedLine struct {
	Old  int
	New  int
	Kind string // "add", "del", "ctx", "meta"
	Text string // includes the leading marker character
}

// RenderedHunk pairs a hunk header with numbered lines.
type RenderedHunk struct {
	Header string
	Lines  []RenderedLine
}

// RenderHunks resolves per-line old/new numbers from hunk headers. Numbers
// stay 0 when a header is unparsable (combined diffs, corrupt input).
func RenderHunks(f DiffFile) []RenderedHunk {
	out := make([]RenderedHunk, 0, len(f.Hunks))
	for _, h := range f.Hunks {
		oldN, newN, ok := parseHunkStarts(h.Header)
		rh := RenderedHunk{Header: h.Header, Lines: make([]RenderedLine, 0, len(h.Lines))}
		for _, l := range h.Lines {
			var rl RenderedLine
			rl.Text = l
			switch {
			case strings.HasPrefix(l, "+"):
				rl.Kind = "add"
				if ok {
					rl.New = newN
					newN++
				}
			case strings.HasPrefix(l, "-"):
				rl.Kind = "del"
				if ok {
					rl.Old = oldN
					oldN++
				}
			case strings.HasPrefix(l, "\\"):
				rl.Kind = "meta"
			default:
				rl.Kind = "ctx"
				if ok {
					rl.Old = oldN
					rl.New = newN
					oldN++
					newN++
				}
			}
			rh.Lines = append(rh.Lines, rl)
		}
		out = append(out, rh)
	}
	return out
}

// parseHunkStarts extracts start lines from "@@ -a[,b] +c[,d] @@ …".
func parseHunkStarts(header string) (oldStart, newStart int, ok bool) {
	if !strings.HasPrefix(header, "@@") {
		return 0, 0, false
	}
	var haveOld, haveNew bool
	for f := range strings.FieldsSeq(header) {
		switch {
		case !haveOld && strings.HasPrefix(f, "-"):
			oldStart, haveOld = parseHunkStart(f[1:])
		case !haveNew && strings.HasPrefix(f, "+"):
			newStart, haveNew = parseHunkStart(f[1:])
		}
		if haveOld && haveNew {
			return oldStart, newStart, true
		}
	}
	return 0, 0, false
}

func parseHunkStart(s string) (int, bool) {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
