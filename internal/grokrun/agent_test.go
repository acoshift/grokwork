package grokrun

import (
	"strings"
	"testing"
)

func TestParseAgent(t *testing.T) {
	cases := []struct {
		in   string
		want Agent
		ok   bool
	}{
		{"", AgentGrok, true},
		{"grok", AgentGrok, true},
		{" GROK ", AgentGrok, true},
		{"claude", AgentClaude, true},
		{"Claude-Code", AgentClaude, true},
		{"cc", AgentClaude, true},
		{"cursor", AgentCursor, true},
		{"cursor-agent", AgentCursor, true},
		{"Cursor-Agent", AgentCursor, true},
		{"gpt", AgentGrok, false},
	}
	for _, c := range cases {
		got, ok := ParseAgent(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseAgent(%q)=%q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The zero value must behave as grok so callers that never set Agent keep working.
func TestAgentZeroValueIsGrok(t *testing.T) {
	var a Agent
	if a.Resolve() != AgentGrok || a.String() != "grok" || a.Label() != "Grok" {
		t.Fatalf("zero value resolved to %q/%q", a.String(), a.Label())
	}
	if _, isGrok := a.driver().(grokDriver); !isGrok {
		t.Fatalf("zero value picked %T", a.driver())
	}
	if _, isClaude := AgentClaude.driver().(claudeDriver); !isClaude {
		t.Fatalf("claude picked %T", AgentClaude.driver())
	}
	if _, isCursor := AgentCursor.driver().(cursorDriver); !isCursor {
		t.Fatalf("cursor picked %T", AgentCursor.driver())
	}
}

// Tool names are per-agent vocabulary: grok's read_file means nothing to claude,
// so an allowlist must never be shared across agents.
func TestAgentToolVocabularyDiffers(t *testing.T) {
	if got := AgentGrok.DefaultInvestigateTools(); got != "read_file,grep,write_file" {
		t.Errorf("grok file tools=%q", got)
	}
	if got := AgentClaude.DefaultInvestigateTools(); got != "Read,Grep,Glob,Write" {
		t.Errorf("claude file tools=%q", got)
	}
	if got := AgentGrok.InvestigateTools(true); got != "read_file,grep,write_file,run_terminal_command" {
		t.Errorf("grok shell tools=%q", got)
	}
	if got := AgentClaude.InvestigateTools(true); got != "Read,Grep,Glob,Write,Bash" {
		t.Errorf("claude shell tools=%q", got)
	}
	if got := AgentCursor.DefaultInvestigateTools(); got != "Read,Grep,Glob" {
		t.Errorf("cursor file tools=%q", got)
	}
	if got := AgentCursor.InvestigateTools(true); got != "Read,Grep,Glob" {
		t.Errorf("cursor has no shell tool; investigate-with-shell=%q", got)
	}
	if AgentGrok.InvestigateTools(true) == AgentClaude.InvestigateTools(true) {
		t.Error("agents must not share a tool allowlist")
	}
}

// The dropdown list and the inference table are two halves of one contract: a
// model offered in the UI must route to the agent the UI claims it does, or the
// user picks "claude-sonnet-5" and the run goes to grok.
func TestModelOptionsMatchInference(t *testing.T) {
	opts := ModelOptions()
	if len(opts) == 0 {
		t.Fatal("no model options")
	}
	seen := map[string]bool{}
	for _, opt := range opts {
		got, ok := AgentForModel(opt.Value)
		if !ok {
			t.Errorf("%q is offered in the UI but AgentForModel does not recognize it", opt.Value)
			continue
		}
		if got != opt.Agent {
			t.Errorf("%q declared agent %q but infers %q", opt.Value, opt.Agent, got)
		}
		if opt.Label == "" {
			t.Errorf("%q has no label", opt.Value)
		}
		if seen[opt.Value] {
			t.Errorf("%q listed twice", opt.Value)
		}
		seen[opt.Value] = true
	}
	if !IsKnownModel("claude-opus-5") || IsKnownModel("not-a-model") || IsKnownModel("opus") {
		t.Error("IsKnownModel disagrees with the option list")
	}
	// Both agents must be represented, or one CLI becomes unselectable in the UI.
	var grok, claude, cursor int
	for _, opt := range opts {
		switch opt.Agent {
		case AgentGrok:
			grok++
		case AgentClaude:
			claude++
		case AgentCursor:
			cursor++
		}
	}
	if grok == 0 || claude == 0 || cursor == 0 {
		t.Fatalf("options cover grok=%d claude=%d cursor=%d; all three must be offered", grok, claude, cursor)
	}
	if opts[0].Value != "grok-4.6-xhigh" || opts[0].Agent != AgentGrok {
		t.Fatalf("first option is %q/%q; newest grok model must lead", opts[0].Value, opts[0].Agent)
	}
}

func TestAgentForModel(t *testing.T) {
	cases := []struct {
		in   string
		want Agent
		ok   bool
	}{
		{"grok-4.6-xhigh", AgentGrok, true},
		{"grok-4.6", AgentGrok, true},
		{"grok-4.5", AgentGrok, true},
		{"grok-code-fast-1", AgentGrok, true},
		{"sonnet", AgentClaude, true},
		{"opus", AgentClaude, true},
		{"haiku", AgentClaude, true},
		{"fable", AgentClaude, true},
		{"claude-opus-4-8", AgentClaude, true},
		{"claude-opus-5", AgentClaude, true},
		{"  SONNET  ", AgentClaude, true},
		// Third-party hosts prefix the vendor.
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", AgentClaude, true},
		// cursor-agent. Cursor-hosted Claude ids carry effort/speed suffixes
		// the Claude CLI does not use; those must not route to claude.
		{"composer-2.5", AgentCursor, true},
		{"claude-opus-5-thinking-high", AgentCursor, true},
		{"claude-opus-5-high", AgentCursor, true},
		{"cursor-grok-4.6-xhigh", AgentCursor, true},
		{"cursor-grok-4.6-high", AgentCursor, true},
		{"gpt-5", AgentCursor, true},
		{"gemini-3-pro", AgentCursor, true},
		{"glm-5.2-high", AgentCursor, true},
		{"glm-5.2-max", AgentCursor, true},
		{"kimi-k3-max", AgentCursor, true},
		{"kimi-k2.7-code", AgentCursor, true},
		// Unlisted effort variants still belong to cursor-agent.
		{"kimi-k3-high", AgentCursor, true},
		{"glm-4.7", AgentCursor, true},
		// Unknown names must not be guessed at — the caller falls back to the
		// configured agent, and the CLI reports the bad model itself.
		{"", AgentGrok, false},
		{"some-self-hosted-42", AgentGrok, false},
	}
	for _, c := range cases {
		got, ok := AgentForModel(c.in)
		if ok != c.ok {
			t.Errorf("AgentForModel(%q) ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("AgentForModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestAgentLimitations(t *testing.T) {
	if got := AgentCursor.Limitations(); got == "" || !containsAll(got, "MCP", "Investigate") {
		t.Errorf("cursor limitations=%q", got)
	}
	if got := AgentGrok.Limitations(); got == "" || !containsAll(got, "Investigate", "MCP") {
		t.Errorf("grok limitations=%q", got)
	}
	if got := AgentClaude.Limitations(); got != "" {
		t.Errorf("claude should have no picker caveat, got %q", got)
	}
	if got := LimitationsForModel("composer-2.5", AgentGrok); got != AgentCursor.Limitations() {
		t.Errorf("composer limitations=%q", got)
	}
	if got := LimitationsForModel("claude-opus-5", AgentGrok); got != "" {
		t.Errorf("claude model limitations=%q", got)
	}
	if got := LimitationsForModel("", AgentCursor); got != AgentCursor.Limitations() {
		t.Errorf("empty model should use fallback, got %q", got)
	}
	if got := LimitationsForModel("not-a-model", AgentGrok); got != AgentGrok.Limitations() {
		t.Errorf("unknown model should use fallback, got %q", got)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestCLIResolvedFillsDefaultBin(t *testing.T) {
	if got := (CLI{Agent: AgentClaude}).Resolved().Bin; got != "claude" {
		t.Errorf("claude bin=%q", got)
	}
	if got := (CLI{Agent: AgentCursor}).Resolved().Bin; got != "cursor-agent" {
		t.Errorf("cursor bin=%q", got)
	}
	if got := (CLI{}).Resolved().Bin; got != "grok" {
		t.Errorf("default bin=%q", got)
	}
	if got := (CLI{Agent: AgentClaude, Bin: "/opt/claude"}).Resolved().Bin; got != "/opt/claude" {
		t.Errorf("explicit bin overwritten: %q", got)
	}
}
