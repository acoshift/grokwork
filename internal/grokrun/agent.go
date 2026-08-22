package grokrun

import "strings"

// Agent selects which coding CLI backs a run. The zero value is AgentGrok so
// existing callers that never set Options.Agent keep their behavior.
type Agent string

const (
	AgentGrok   Agent = "grok"
	AgentClaude Agent = "claude"
	AgentCursor Agent = "cursor"
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
	case "cursor", "cursor-agent":
		return AgentCursor, true
	default:
		return AgentGrok, false
	}
}

// KnownAgents is the closed set ParseAgent accepts, for error messages.
func KnownAgents() string {
	return `"` + string(AgentGrok) + `", "` + string(AgentClaude) + `", or "` + string(AgentCursor) + `"`
}

// claudeModelMarkers are substrings that identify a Claude model. The CLI's own
// aliases are bare words ("opus", "sonnet", "fable"), and third-party hosts use
// vendor-prefixed ids ("us.anthropic.claude-sonnet-4-5-…"), so both shapes match.
var claudeModelMarkers = []string{"claude", "anthropic", "opus", "sonnet", "haiku", "fable"}

// cursorModelMarkers identify names that belong to cursor-agent. Checked before
// grok/claude because the catalog reuses those vendors' names
// (cursor-grok-4.6-high, claude-opus-5-thinking-high, glm-5.2-high, kimi-k3-max).
var cursorModelMarkers = []string{"composer", "cursor-", "gpt-", "codex", "gemini", "glm", "kimi"}

// cursorClaudeQualifiers are suffixes the Cursor catalog adds to Claude family
// ids. The Claude CLI's curated names (claude-opus-5, claude-haiku-4-5) do not
// carry these, so a name with both a Claude marker and a qualifier is Cursor's.
var cursorClaudeQualifiers = []string{"thinking", "-fast", "-low", "-medium", "-high", "-xhigh", "-max"}

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
	// Curated names win first so a listed Cursor Claude id cannot be stolen by
	// the Claude-marker heuristic, and a listed Claude id cannot be stolen by a
	// qualifier that happens to appear in some other spelling.
	for _, opt := range ModelOptions() {
		if strings.EqualFold(opt.Value, m) {
			return opt.Agent, true
		}
	}
	if looksLikeCursorModel(m) {
		return AgentCursor, true
	}
	// grok before Claude so a vendor-qualified name that happens to contain a
	// Claude marker still resolves to grok. cursor-grok-* already matched above.
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

func looksLikeCursorModel(m string) bool {
	for _, marker := range cursorModelMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	if !hasClaudeMarker(m) {
		return false
	}
	for _, q := range cursorClaudeQualifiers {
		if strings.Contains(m, q) {
			return true
		}
	}
	return false
}

func hasClaudeMarker(m string) bool {
	for _, marker := range claudeModelMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
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
// The CLIs also accept names not listed here; the config page keeps whatever
// is already in config.json as a selectable option, so a hand-edited value
// survives a save.
func ModelOptions() []ModelOption {
	return []ModelOption{
		// grok. `grok models` reports what a given account can actually reach, so
		// this list is intentionally short — extend it per deployment.
		{Value: "grok-4.6-xhigh", Label: "grok-4.6-xhigh", Agent: AgentGrok},
		{Value: "grok-4.6", Label: "grok-4.6", Agent: AgentGrok},
		{Value: "grok-4.5", Label: "grok-4.5", Agent: AgentGrok},

		// claude (Claude Code CLI).
		{Value: "claude-opus-5", Label: "claude-opus-5", Agent: AgentClaude},
		{Value: "claude-opus-4-8", Label: "claude-opus-4-8", Agent: AgentClaude},
		{Value: "claude-sonnet-5", Label: "claude-sonnet-5", Agent: AgentClaude},
		{Value: "claude-haiku-4-5", Label: "claude-haiku-4-5", Agent: AgentClaude},
		{Value: "claude-fable-5", Label: "claude-fable-5", Agent: AgentClaude},

		// cursor-agent. Claude-family ids here are Cursor's effort/speed variants,
		// distinct from the Claude Code names above — picking one is how a start
		// task runs Claude-quality models on cursor-agent.
		{Value: "composer-2.5", Label: "composer-2.5", Agent: AgentCursor},
		{Value: "composer-2.5-fast", Label: "composer-2.5-fast", Agent: AgentCursor},
		{Value: "claude-opus-5-thinking-high", Label: "claude-opus-5-thinking-high", Agent: AgentCursor},
		{Value: "claude-sonnet-5-thinking-high", Label: "claude-sonnet-5-thinking-high", Agent: AgentCursor},
		{Value: "claude-fable-5-thinking-high", Label: "claude-fable-5-thinking-high", Agent: AgentCursor},
		{Value: "gpt-5.6-sol-medium", Label: "gpt-5.6-sol-medium", Agent: AgentCursor},
		{Value: "cursor-grok-4.6-xhigh", Label: "cursor-grok-4.6-xhigh", Agent: AgentCursor},
		{Value: "cursor-grok-4.6-high", Label: "cursor-grok-4.6-high", Agent: AgentCursor},
		{Value: "gemini-3.7-flash-high", Label: "gemini-3.7-flash-high", Agent: AgentCursor},
		{Value: "glm-5.2-high", Label: "glm-5.2-high", Agent: AgentCursor},
		{Value: "glm-5.2-max", Label: "glm-5.2-max", Agent: AgentCursor},
		{Value: "kimi-k3-max", Label: "kimi-k3-max", Agent: AgentCursor},
		{Value: "kimi-k2.7-code", Label: "kimi-k2.7-code", Agent: AgentCursor},
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
	case AgentCursor:
		return "Cursor"
	default:
		return "Grok"
	}
}

// Limitations is the operator-facing caveat for this CLI, shown next to the
// model picker. Empty means the harness has nothing the picker must warn
// about. Keep this in lockstep with mcpCapsForRun and the driver flags:
// cursor-agent has no --mcp-config, and grok investigate cannot allow one
// named MCP server (--deny MCPTool) unless the project sets agentMCPAlways.
func (a Agent) Limitations() string {
	switch a.Resolve() {
	case AgentCursor:
		return "Cannot use grokwork MCP (tickets, Linear, ClickUp, storage). Investigate is read-only ask mode — no tool allowlist and no shell."
	case AgentGrok:
		return "Investigate cannot use grokwork MCP (the CLI cannot allow one named server). Ship and Fix still attach MCP."
	default:
		return ""
	}
}

// LimitationsForModel is Limitations of the CLI that owns model, or of
// fallback when the name identifies neither CLI.
func LimitationsForModel(model string, fallback Agent) string {
	if a, ok := AgentForModel(model); ok {
		return a.Limitations()
	}
	return fallback.Limitations()
}

// SessionLabel names the durable session in user-facing copy ("the Grok
// session", "the Claude session").
func (a Agent) SessionLabel() string { return a.Label() + " session" }

// DefaultInvestigateTools is the file-only allowlist for investigate runs.
// Tool names are agent-specific vocabulary and are not interchangeable.
// Shell is added separately when the actor may use diagnostics (see
// InvestigateTools). Project investigateTools may override for grok when shell
// is allowed.
func (a Agent) DefaultInvestigateTools() string {
	switch a.Resolve() {
	case AgentClaude:
		return "Read,Grep,Glob"
	case AgentCursor:
		// cursor-agent has no --tools flag; the driver maps a non-nil Tools
		// pointer to --mode ask. These names are for policy/display only.
		return "Read,Grep,Glob"
	default:
		return "read_file,grep"
	}
}

// ShellInvestigateTool is the agent-specific shell tool name, or empty if none.
func (a Agent) ShellInvestigateTool() string {
	switch a.Resolve() {
	case AgentClaude:
		return "Bash"
	case AgentCursor:
		// Ask mode is read-only, so investigate-with-shell still cannot open a
		// write path. Empty keeps InvestigateTools(true) file-only.
		return ""
	default:
		return "run_terminal_command"
	}
}

// InvestigateTools returns the investigate allowlist for this agent.
// When shell is true, the diagnostic shell tool is included (not a sandbox —
// mutate intent stays prompt-enforced; GH_TOKEN is still omitted by policy).
func (a Agent) InvestigateTools(shell bool) string {
	base := a.DefaultInvestigateTools()
	if !shell {
		return base
	}
	sh := a.ShellInvestigateTool()
	if sh == "" {
		return base
	}
	return base + "," + sh
}

// DefaultBin is the binary name looked up on PATH when config leaves it unset.
func (a Agent) DefaultBin() string {
	switch a.Resolve() {
	case AgentClaude:
		return "claude"
	case AgentCursor:
		return "cursor-agent"
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
	case AgentCursor:
		return cursorDriver{}
	default:
		return grokDriver{}
	}
}
