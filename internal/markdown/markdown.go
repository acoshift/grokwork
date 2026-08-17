// Package markdown renders GitHub-flavored markdown for the web UI.
package markdown

import (
	"bytes"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldhtml "github.com/yuin/goldmark/renderer/html"
)

// gfm mirrors how GitHub renders issue/PR bodies: tables, strikethrough,
// autolinks, task lists, and comment-style hard line breaks. Raw HTML and
// javascript: URLs are dropped (goldmark safe default) — bodies come from
// GitHub/Linear and are untrusted. HTML <img> is lifted first because GitHub
// pastes screenshots as <img>, not markdown, and goldmark would omit them.
var gfm = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(goldhtml.WithHardWraps()),
)

var (
	// GitHub sometimes wraps a pasted <img> in <a href> to the same asset.
	wrappedHTMLImg = regexp.MustCompile(`(?is)<a\b[^>]*>\s*(<img\b[^>]*>)\s*</a>`)
	htmlImgTag     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	htmlAttr       = regexp.MustCompile(`(?i)\b([^\s=/>]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// Render converts markdown to sanitized HTML for direct template embedding.
func Render(src string) template.HTML {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	src = liftHTMLImages(src)
	var buf bytes.Buffer
	if err := gfm.Convert([]byte(src), &buf); err != nil {
		// Never emit unescaped source.
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(rewriteImages(buf.String()))
}

func liftHTMLImages(src string) string {
	src = wrappedHTMLImg.ReplaceAllString(src, "$1")
	return htmlImgTag.ReplaceAllStringFunc(src, func(tag string) string {
		img, ok := parseSafeImg(tag)
		if !ok {
			return ""
		}
		alt := strings.NewReplacer(
			"[", `\[`,
			"]", `\]`,
			"\r", " ",
			"\n", " ",
		).Replace(img.alt)
		return "![" + alt + "](<" + img.src + ">)"
	})
}

type safeImg struct {
	src string
	alt string
}

func parseSafeImg(tag string) (safeImg, bool) {
	attrs := parseAttrs(tag)
	src, ok := safeImageURL(attrs["src"])
	if !ok {
		return safeImg{}, false
	}
	return safeImg{src: src, alt: html.UnescapeString(attrs["alt"])}, true
}

func parseAttrs(tag string) map[string]string {
	out := make(map[string]string)
	for _, m := range htmlAttr.FindAllStringSubmatch(tag, -1) {
		out[strings.ToLower(m[1])] = m[2] + m[3] + m[4]
	}
	return out
}

func safeImageURL(raw string) (string, bool) {
	raw = html.UnescapeString(strings.TrimSpace(raw))
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	if u.Host == "" || u.User != nil {
		return "", false
	}
	s := u.String()
	if strings.ContainsAny(s, "<>") {
		return "", false
	}
	return s, true
}

func rewriteImages(s string) string {
	return htmlImgTag.ReplaceAllStringFunc(s, func(tag string) string {
		img, ok := parseSafeImg(tag)
		if !ok {
			return ""
		}
		return `<img src="` + template.HTMLEscapeString(img.src) +
			`" alt="` + template.HTMLEscapeString(img.alt) +
			`" referrerpolicy="no-referrer" loading="lazy">`
	})
}
