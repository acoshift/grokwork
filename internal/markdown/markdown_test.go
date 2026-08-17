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

func TestRenderGitHubHTMLImage(t *testing.T) {
	src := "see\n\n<img width=\"1552\" height=\"1014\" alt=\"repro\" src=\"https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\" />\n"
	got := string(Render(src))
	for _, want := range []string{
		`<img src="https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`,
		`alt="repro"`,
		`referrerpolicy="no-referrer"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "onerror") || strings.Contains(got, "width=") {
		t.Fatalf("must not pass through raw HTML attrs: %s", got)
	}
}

func TestRenderWrappedGitHubHTMLImage(t *testing.T) {
	src := `<a href="https://github.com/user-attachments/assets/abcd"><img src="https://github.com/user-attachments/assets/abcd" alt="shot"></a>`
	got := string(Render(src))
	if !strings.Contains(got, `<img src="https://github.com/user-attachments/assets/abcd"`) {
		t.Fatalf("wrapped html img must render: %s", got)
	}
	if !strings.Contains(got, `alt="shot"`) {
		t.Fatalf("alt missing: %s", got)
	}
}

func TestRenderMarkdownImage(t *testing.T) {
	got := string(Render("![diagram](https://example.com/a.png)"))
	if !strings.Contains(got, `<img src="https://example.com/a.png"`) || !strings.Contains(got, `alt="diagram"`) {
		t.Fatalf("markdown image must render: %s", got)
	}
}

func TestRenderDropsUnsafeImageURL(t *testing.T) {
	got := string(Render("![](javascript:alert(1))\n\n<img src=\"data:image/png;base64,abc\" alt=\"x\">\n\n<img src=\"https://example.com/ok.png\">"))
	if strings.Contains(got, "javascript:") || strings.Contains(got, "data:") {
		t.Fatalf("unsafe image url leaked: %s", got)
	}
	if !strings.Contains(got, `<img src="https://example.com/ok.png"`) {
		t.Fatalf("safe sibling image must still render: %s", got)
	}
}

func TestRenderRewriting(t *testing.T) {
	got := string(RenderRewriting(
		"![x](https://github.com/user-attachments/assets/abcd)\n\n![y](https://example.com/a.png)",
		func(u string) string {
			if strings.Contains(u, "github.com") {
				return "/projects/p/github-images?u=" + u
			}
			return u
		},
	))
	if !strings.Contains(got, `src="/projects/p/github-images?u=https://github.com/user-attachments/assets/abcd"`) {
		t.Fatalf("github url must rewrite: %s", got)
	}
	if !strings.Contains(got, `src="https://example.com/a.png"`) {
		t.Fatalf("non-github url must stay: %s", got)
	}
}

func TestRenderImageInCodeFenceStaysText(t *testing.T) {
	got := string(Render("```\n<img src=\"https://example.com/a.png\">\n```"))
	if strings.Contains(got, "<img src=") {
		t.Fatalf("fenced html img must not become an image: %s", got)
	}
}
