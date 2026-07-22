package markdown

import (
	"strings"
	"testing"
)

func TestRenderGFM(t *testing.T) {
	got := string(Render("## What\n\nfix **race** in `queue`\n\n- [x] unit\n- [ ] e2e\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nx := 1\n```\n\nhttps://example.com/pr"))
	for _, want := range []string{
		"<h2>What</h2>",
		"<strong>race</strong>",
		"<code>queue</code>",
		`type="checkbox"`,
		"<table>",
		"<td>1</td>",
		"<pre><code",
		`<a href="https://example.com/pr"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

func TestRenderHardWraps(t *testing.T) {
	got := string(Render("line one\nline two"))
	if !strings.Contains(got, "<br") {
		t.Fatalf("single newline must hard-break like GitHub comments: %s", got)
	}
}

func TestRenderSafe(t *testing.T) {
	got := string(Render("<script>alert(1)</script>\n\n[x](javascript:alert(1))\n\n<img src=x onerror=alert(1)>"))
	for _, bad := range []string{"<script", "javascript:", "onerror"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unsafe %q leaked: %s", bad, got)
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := Render("  \n "); got != "" {
		t.Fatalf("empty input must render empty, got %q", got)
	}
}
