package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFooterTabsShareNavLevel pins the phone footer contract: every footer
// tab that is a URL segment must be a TAB_SECTIONS peer. navLevel treats
// those as level 1, so switching among them crossfades. A tab left out of
// the set (Today, before it was added) is 1+len(segments) and slides instead.
// Overview and Projects are level 1 through the length checks, not the set.
func TestFooterTabsShareNavLevel(t *testing.T) {
	raw, err := os.ReadFile("templates/layout.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := string(raw)

	_, after, ok := strings.Cut(tmpl, `id="tab-bar"`)
	if !ok {
		t.Fatal("tab-bar missing from layout")
	}
	bar, _, ok := strings.Cut(after, "</div>")
	if !ok {
		t.Fatal("tab-bar unclosed")
	}
	icons := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`data-icon="([^"]+)"`).FindAllStringSubmatch(bar, -1) {
		icons[m[1]] = struct{}{}
	}
	if len(icons) == 0 {
		t.Fatal("tab bar has no data-icon")
	}

	_, tableRest, ok := strings.Cut(tmpl, "var TAB_SECTIONS = {")
	if !ok {
		t.Fatal("TAB_SECTIONS missing")
	}
	table, _, ok := strings.Cut(tableRest, "}")
	if !ok {
		t.Fatal("TAB_SECTIONS unclosed")
	}

	// Not path segments. navLevel returns 1 for "/" and for /projects/{p}.
	skip := map[string]bool{"overview": true, "projects": true}
	for icon := range icons {
		if skip[icon] {
			continue
		}
		// A bare Contains would treat "view" as present inside "reviews:".
		ok, err := regexp.MatchString(`(?:^|[{,\s])`+regexp.QuoteMeta(icon)+`\s*:`, table)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("footer tab %q missing from TAB_SECTIONS (%s); it slides while the other footer tabs crossfade", icon, strings.TrimSpace(table))
		}
	}
	if _, ok := icons["today"]; !ok {
		t.Fatal("footer no longer includes Today")
	}
	if _, ok := icons["overview"]; !ok {
		t.Fatal("footer no longer includes Overview")
	}
	if !strings.Contains(tmpl, "if (!seg.length) return 1") || !strings.Contains(tmpl, "if (seg.length === 2) return 1") {
		t.Fatal("Projects (/) or Overview (/projects/{p}) lost the level-1 shortcut")
	}
}
