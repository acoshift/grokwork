package web

import (
	"html"
	"strings"
	"testing"
)

func TestHighlightGoKeywordAndString(t *testing.T) {
	lines := highlightSource("src/main.go", "package main\n\treturn \"hi\"\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d", len(lines))
	}
	kw := tokenTexts(lines, tokKW)
	str := tokenTexts(lines, tokStr)
	if !strings.Contains(strings.Join(kw, " "), "package") || !strings.Contains(strings.Join(kw, " "), "return") {
		t.Fatalf("keywords %q", kw)
	}
	if len(str) != 1 || str[0] != `"hi"` {
		t.Fatalf("strings %q", str)
	}
	if tokKW == tokStr {
		t.Fatal("keyword and string share a class")
	}
}

func TestHighlightEscapesScriptText(t *testing.T) {
	src := "var s = \"<script>alert(1)</script>\"\n"
	lines := highlightSource("main.go", src)
	var raw strings.Builder
	var sawStr bool
	for _, ln := range lines {
		for _, tok := range ln.Tokens {
			raw.WriteString(tok.Text)
			if strings.Contains(tok.Text, "<script>") {
				sawStr = true
				if tok.Class != tokStr {
					t.Fatalf("script text class %q", tok.Class)
				}
				esc := html.EscapeString(tok.Text)
				if !strings.Contains(esc, "&lt;script&gt;") || strings.Contains(esc, "<script>") {
					t.Fatalf("escaped %q", esc)
				}
			}
		}
	}
	if !sawStr {
		t.Fatal("script text was dropped")
	}
	if raw.String() != strings.TrimSuffix(src, "\n") {
		t.Fatalf("tokens changed the source: %q", raw.String())
	}
}

func TestHighlightUnknownStaysPlain(t *testing.T) {
	lines := highlightSource("notes.txt", "package \"hi\"\n")
	if len(lines) != 1 || len(lines[0].Tokens) != 1 {
		t.Fatalf("%+v", lines)
	}
	tok := lines[0].Tokens[0]
	if tok.Class != "" || tok.Text != `package "hi"` {
		t.Fatalf("%+v", tok)
	}
	if strings.Contains(tok.Class, tokKW) || strings.Contains(tok.Class, tokStr) {
		t.Fatal("unknown extension was colored")
	}
}

func TestHighlightEmptyFile(t *testing.T) {
	if lines := highlightSource("main.go", ""); lines != nil {
		t.Fatalf("empty: %+v", lines)
	}
	if lines := highlightSource("main.go", "\n"); lines != nil {
		t.Fatalf("blank: %+v", lines)
	}
}

func TestHighlightDoesNotPaintOverLineCap(t *testing.T) {
	body := strings.Repeat("package main\n", codeMaxLines+1)
	if lines := highlightSource("main.go", body); lines != nil {
		t.Fatalf("over cap painted %d lines", len(lines))
	}
}

func tokenTexts(lines []codeLine, class string) []string {
	var out []string
	for _, ln := range lines {
		for _, tok := range ln.Tokens {
			if tok.Class == class {
				out = append(out, tok.Text)
			}
		}
	}
	return out
}
