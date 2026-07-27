package grokrun

import "strings"

// Agent selects which coding CLI backs a run. The zero value is AgentGrok so
// existing callers that never set Options.Agent keep their behavior.
type Agent string

const (
	AgentGrok   Agent = "grok"
	AgentClaude Agent = "claude"
)

// ParseAgent normalizes user/config input. Empty means "unset" and resolves to
// AgentGrok; ok is false for anything unrecognized so callers can reject typos
// instead of silently falling back.
func ParseAgent(s string) (Agent, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "grok":
		return AgentGrok, true
	case "claude", "cc", "claude-code":
		return AgentClaude, true
	default:
		return AgentGrok, false
	}
}

// claudeModelMarkers are substrings that identify a Claude model. The CLI's own
// aliases are bare words ("opus", "sonnet", "fable"), and third-party hosts use
// vendor-prefixed ids ("us.anthropic.claude-sonnet-4-5-…"), so both shapes match.
var claudeModelMarkers = []string{"claude", "anthropic", "opus", "sonnet", "haiku", "fable"}

// AgentForModel infers which CLI owns a model name.
//
// This is a heuristic over names neither vendor guarantees to keep
// distinguishable — the Claude aliases in particular are generic words. So an
// unrecognized name returns ok=false rather than guessing, and callers fall back
// to the explicitly configured agent. Never treat !ok as "grok".
func AgentForModel(model string) (Agent, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return "", false
	}
	// grok is checked first so a vendor-qualified name that happens to contain a
	// Claude marker still resolves to grok.
	if strings.Contains(m, "grok") {
		return AgentGrok, true
	}
	for _, marker := range claudeModelMarkers {
		if strings.Contains(m, marker) {
			return AgentClaude, true
		}
	}
	return "", false
}

// ModelOption is one selectable model for the config UI.
type ModelOption struct {
	// Value is passed to the CLI's model flag verbatim.
	Value string
	Label string
	// Agent is the CLI this name belongs to, and must match what AgentForModel
	// infers — TestModelOptionsMatchInference pins that.
	Agent Agent
}

// ModelOptions is the curated list offered in the config UI, newest first per
// agent. It lives next to AgentForModel so the two cannot drift: a name added
// here that the inference table does not recognize fails the tests.
//
// Only pinned vendor ids are listed — no rolling aliases like "opus"/"sonnet".
// Both CLIs also accept names not listed here; the config page keeps whatever
// is already in config.json as a selectable option, so a hand-edited value
// survives a save.
func ModelOptions() []ModelOption {
	return []ModelOption{
		// grok. `grok models` reports what a given account can actually reach, so
		// this list is intentionally short — extend it per deployment.
		{Value: "grok-4.5", Label: "grok-4.5", Agent: AgentGrok},

		// claude.
		{Value: "claude-opus-5", Label: "claude-opus-5", Agent: AgentClaude},
		{Value: "claude-opus-4-8", Label: "claude-opus-4-8", Agent: AgentClaude},
		{Value: "claude-sonnet-5", Label: "claude-sonnet-5", Agent: AgentClaude},
		{Value: "claude-haiku-4-5", Label: "claude-haiku-4-5", Agent: AgentClaude},
		{Value: "claude-fable-5", Label: "claude-fable-5", Agent: AgentClaude},
	}
}

// IsKnownModel reports whether name is one of the curated options.
func IsKnownModel(name string) bool {
	name = strings.TrimSpace(name)
	for _, opt := range ModelOptions() {
		if opt.Value == name {
			return true
		}
	}
	return false
}

// Resolve maps the zero value to the default agent.
func (a Agent) Resolve() Agent {
	if a == "" {
		return AgentGrok
	}
	return a
}

func (a Agent) String() string { return string(a.Resolve()) }

// Label is the display name for Discord cards and the web UI.
func (a Agent) Label() string {
	switch a.Resolve() {
	case AgentClaude:
		return "Claude"
	default:
		return "Grok"
	}
}

// SessionLabel names the durable session in user-facing copy ("the Grok
// session", "the Claude session").
func (a Agent) SessionLabel() string { return a.Label() + " session" }

// DefaultInvestigateTools is the allowlist for investigate runs: file inspection
// plus a shell tool so agents can run diagnostic CLIs (psql, dig, curl, …).
// Tool names are agent-specific vocabulary and are not interchangeable.
// Shell is not a sandbox — mutate intent stays prompt-enforced (no commits/PRs;
// GH_TOKEN is still omitted). Project investigateTools may override for grok.
func (a Agent) DefaultInvestigateTools() string {
	switch a.Resolve() {
	case AgentClaude:
		return "Read,Grep,Glob,Bash"
	default:
		return "read_file,grep,run_terminal_command"
	}
}

// DefaultBin is the binary name looked up on PATH when config leaves it unset.
func (a Agent) DefaultBin() string {
	switch a.Resolve() {
	case AgentClaude:
		return "claude"
	default:
		return "grok"
	}
}

// CLI is the resolved binary and model for one agent. Agent, binary, and model
// name always travel together — a grok model name is meaningless to claude and
// vice versa — so helpers take this instead of three loose strings.
type CLI struct {
	Agent Agent
	Bin   string
	Model string
}

// Resolved fills in the default binary when Bin is unset.
func (c CLI) Resolved() CLI {
	c.Agent = c.Agent.Resolve()
	if strings.TrimSpace(c.Bin) == "" {
		c.Bin = c.Agent.DefaultBin()
	}
	return c
}

func (a Agent) driver() driver {
	switch a.Resolve() {
	case AgentClaude:
		return claudeDriver{}
	default:
		return grokDriver{}
	}
}
