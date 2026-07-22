// Package markdown renders GitHub-flavored markdown for the web UI.
package markdown

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// gfm mirrors how GitHub renders issue/PR bodies: tables, strikethrough,
// autolinks, task lists, and comment-style hard line breaks. Raw HTML and
// javascript: URLs are dropped (goldmark safe default) — bodies come from
// GitHub/Linear and are untrusted.
var gfm = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// Render converts markdown to sanitized HTML for direct template embedding.
func Render(src string) template.HTML {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := gfm.Convert([]byte(src), &buf); err != nil {
		// Never emit unescaped source.
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(buf.String())
}
