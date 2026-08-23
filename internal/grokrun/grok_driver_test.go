package grokrun

import (
	"slices"
	"testing"
)

func TestGrokCLIModel(t *testing.T) {
	cases := []struct {
		in, model, effort string
	}{
		{"grok-4.6-xhigh", "grok-4.6", "xhigh"},
		{" GROK-4.6-XHIGH ", "grok-4.6", "xhigh"},
		{"grok-4.6", "grok-4.6", ""},
		{"grok-4.5", "grok-4.5", ""},
		// Cursor's xhigh id is a real catalog name and must not be rewritten
		// if it ever reaches this helper.
		{"cursor-grok-4.6-xhigh", "cursor-grok-4.6-xhigh", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		model, effort := grokCLIModel(c.in)
		if model != c.model || effort != c.effort {
			t.Errorf("grokCLIModel(%q)=%q,%q want %q,%q", c.in, model, effort, c.model, c.effort)
		}
	}
}

func TestGrokArgsXhighMapsToEffort(t *testing.T) {
	args := grokArgs(Options{Model: "grok-4.6-xhigh", MaxTurns: 1})
	if got := argValue(args, "-m"); got != "grok-4.6" {
		t.Errorf("-m=%q want grok-4.6 in %v", got, args)
	}
	if got := argValue(args, "--effort"); got != "xhigh" {
		t.Errorf("--effort=%q want xhigh in %v", got, args)
	}
}

func TestGrokArgsPlainModelOmitsEffort(t *testing.T) {
	args := grokArgs(Options{Model: "grok-4.6", MaxTurns: 1})
	if got := argValue(args, "-m"); got != "grok-4.6" {
		t.Errorf("-m=%q in %v", got, args)
	}
	if slices.Contains(args, "--effort") {
		t.Errorf("plain grok-4.6 must not pass --effort: %v", args)
	}
}
