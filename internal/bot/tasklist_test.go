package bot

import (
	"strings"
	"testing"
)

func TestParseTasklistBasic(t *testing.T) {
	body := "## Breakdown\n<!-- grokwork:tasklist -->\n- [ ] first sub-task\n- [x] done one\n* [ ] star bullet\n"
	items := ParseTasklist(body)
	if len(items) != 3 {
		t.Fatalf("len=%d items=%+v", len(items), items)
	}
	if items[0].Text != "first sub-task" || items[0].Checked || items[0].RawLine != "- [ ] first sub-task" {
		t.Fatalf("item0=%+v", items[0])
	}
	if items[1].Text != "done one" || !items[1].Checked {
		t.Fatalf("item1=%+v", items[1])
	}
	if items[2].Text != "star bullet" || items[2].RawLine != "* [ ] star bullet" {
		t.Fatalf("item2=%+v", items[2])
	}
}

func TestParseTasklistIgnoresFences(t *testing.T) {
	body := "" +
		"- [ ] real item\n" +
		"```\n" +
		"- [ ] inside backticks\n" +
		"```\n" +
		"~~~\n" +
		"- [x] inside tildes\n" +
		"~~~\n" +
		"- [X] uppercase checked\n"
	items := ParseTasklist(body)
	if len(items) != 2 {
		t.Fatalf("len=%d items=%+v", len(items), items)
	}
	if items[0].Text != "real item" || items[1].Text != "uppercase checked" || !items[1].Checked {
		t.Fatalf("%+v", items)
	}
	// Nested wrong fence char must not close.
	body2 := "```\n- [ ] still fenced\n~~~\n- [ ] still fenced 2\n```\n- [ ] outside\n"
	items2 := ParseTasklist(body2)
	if len(items2) != 1 || items2[0].Text != "outside" {
		t.Fatalf("%+v", items2)
	}
}

func TestParseTasklistLeadingWhitespace(t *testing.T) {
	body := "  - [ ] indented\n\t* [ ] tabbed\n"
	items := ParseTasklist(body)
	if len(items) != 2 || items[0].Text != "indented" || items[1].Text != "tabbed" {
		t.Fatalf("%+v", items)
	}
}

func TestAnnotateAndParseRoundTrip(t *testing.T) {
	body := "## Breakdown\n- [ ] ship the widget\n- [ ] write docs\n"
	raw := "- [ ] ship the widget"
	url := "https://grokwork.example/sessions/w_abc123?project=app"
	newBody, ok := AnnotateTasklistLine(body, raw, url)
	if !ok {
		t.Fatal("expected change")
	}
	items := ParseTasklist(newBody)
	if len(items) != 2 {
		t.Fatalf("%+v", items)
	}
	if items[0].SessionURL != url {
		t.Fatalf("SessionURL=%q", items[0].SessionURL)
	}
	if items[0].ThreadID != "w_abc123" {
		t.Fatalf("ThreadID=%q", items[0].ThreadID)
	}
	if items[0].Text != "ship the widget" {
		t.Fatalf("Text=%q", items[0].Text)
	}
	// Second item untouched.
	if items[1].SessionURL != "" || items[1].Text != "write docs" {
		t.Fatalf("%+v", items[1])
	}
}

func TestAnnotateDuplicateRawLineOnlyFirst(t *testing.T) {
	body := "- [ ] same\n- [ ] same\n"
	raw := "- [ ] same"
	newBody, ok := AnnotateTasklistLine(body, raw, "https://x/sessions/1")
	if !ok {
		t.Fatal("expected change")
	}
	items := ParseTasklist(newBody)
	if items[0].SessionURL == "" || items[1].SessionURL != "" {
		t.Fatalf("%+v", items)
	}
	// Second annotate hits the second line.
	newBody2, ok := AnnotateTasklistLine(newBody, raw, "https://x/sessions/2")
	if !ok {
		t.Fatal("expected second change")
	}
	items2 := ParseTasklist(newBody2)
	if items2[0].ThreadID != "1" || items2[1].ThreadID != "2" {
		t.Fatalf("%+v", items2)
	}
}

func TestAnnotateNoMatch(t *testing.T) {
	body := "- [ ] a\n"
	got, ok := AnnotateTasklistLine(body, "- [ ] missing", "https://x/sessions/1")
	if ok || got != body {
		t.Fatalf("ok=%v got=%q", ok, got)
	}
}

func TestCheckTasklistLineFlipsMatching(t *testing.T) {
	body := "" +
		"- [ ] other — [session](https://x/sessions/w_other)\n" +
		"- [ ] mine — [session](https://x/sessions/w_mine?project=p)\n" +
		"- [x] already — [session](https://x/sessions/w_mine)\n"
	newBody, ok := CheckTasklistLine(body, "w_mine")
	if !ok {
		t.Fatal("expected change")
	}
	items := ParseTasklist(newBody)
	if items[0].Checked {
		t.Fatal("other must stay unchecked")
	}
	if !items[1].Checked || items[1].ThreadID != "w_mine" {
		t.Fatalf("mine: %+v", items[1])
	}
	// Already checked line left alone (and we flipped the unchecked one).
	if !items[2].Checked {
		t.Fatalf("%+v", items[2])
	}
	// No-match
	if _, ok := CheckTasklistLine(body, "w_absent"); ok {
		t.Fatal("absent thread must not change")
	}
}

func TestTasklistCRLFPreserved(t *testing.T) {
	body := "- [ ] one\r\n- [ ] two\r\n"
	raw := "- [ ] one"
	newBody, ok := AnnotateTasklistLine(body, raw, "https://x/sessions/th1")
	if !ok {
		t.Fatal("expected change")
	}
	if !strings.Contains(newBody, "\r\n") {
		t.Fatalf("lost CRLF: %q", newBody)
	}
	// Both lines still CRLF-terminated.
	if !strings.HasSuffix(strings.Split(newBody, "\n")[0], "\r") {
		t.Fatalf("first line ending: %q", newBody)
	}
	checked, ok := CheckTasklistLine(newBody, "th1")
	if !ok {
		t.Fatal("check expected change")
	}
	if !strings.Contains(checked, "\r\n") {
		t.Fatalf("check lost CRLF: %q", checked)
	}
	if !strings.Contains(checked, "- [x] one — [session](https://x/sessions/th1)\r\n") {
		t.Fatalf("checked body=%q", checked)
	}
}

func TestThreadIDFromSessionURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://h/sessions/w_abc?project=p", "w_abc"},
		{"https://h/sessions/123456789012345678", "123456789012345678"},
		{"https://h/sessions/w_x#frag", "w_x"},
		{"https://h/other", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := threadIDFromSessionURL(tc.in); got != tc.want {
			t.Fatalf("%q → %q want %q", tc.in, got, tc.want)
		}
	}
}
