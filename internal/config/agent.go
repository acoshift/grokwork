package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/grokrun"
)

// AgentCLI is the resolved invocation for one agent: which binary to exec, which
// model to ask for, and which extra flags to append.
type AgentCLI struct {
	Agent     grokrun.Agent
	Bin       string
	Model     string
	ExtraArgs []string
}

// CLI is the grokrun view of this invocation.
func (a AgentCLI) CLI() grokrun.CLI {
	return grokrun.CLI{Agent: a.Agent, Bin: a.Bin, Model: a.Model}
}

// Model settings are shared across agents: `model` and `summarizeModel` hold a
// name from either vendor, and grokrun.AgentForModel decides which CLI it
// belongs to. Two rules keep that from doing damage:
//
//  1. A name it does not recognize never guesses — it falls back to the
//     configured `agent`, and the CLI itself reports an unknown model loudly.
//  2. Inference only chooses the agent for sessions that have not stamped one.
//     A stamped session keeps its CLI (its session id is not portable), and a
//     model belonging to a different agent is simply not passed, so that run
//     uses its own CLI's default model. Editing `model` therefore never
//     invalidates an existing thread's transcript.

// DefaultAgent is the agent for sessions that have not stamped one: the agent
// that owns `model`, or the explicitly configured `agent` when the model name
// identifies nothing.
func (c *Config) DefaultAgent() grokrun.Agent {
	if c == nil {
		return grokrun.AgentGrok
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.defaultAgentLocked()
}

func (c *Config) defaultAgentLocked() grokrun.Agent {
	if a, ok := grokrun.AgentForModel(c.Model); ok {
		return a
	}
	agent, _ := grokrun.ParseAgent(c.Agent)
	return agent
}

// ModelAgent reports which agent owns the configured task model, and whether the
// name identified one at all. The web UI shows this so the derived agent is never
// a mystery.
func (c *Config) ModelAgent() (grokrun.Agent, bool) {
	if c == nil {
		return grokrun.AgentGrok, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return grokrun.AgentForModel(c.Model)
}

// SummarizeModelAgent is ModelAgent for the thread-title model.
func (c *Config) SummarizeModelAgent() (grokrun.Agent, bool) {
	if c == nil {
		return grokrun.AgentGrok, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return grokrun.AgentForModel(c.SummarizeModel)
}

// TaskModel is the configured task model. Empty means "let the CLI pick".
func (c *Config) TaskModel() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Model)
}

// EffectiveReviewModel is the model a review session runs on when the dispatch UI
// does not name one: the configured review model, else the task model. Empty means
// "let the CLI pick", same as Model.
//
// It must agree with ReviewAgentCLI("") — this is what the dispatch modal renders as
// its "Default (…)" option, and a label naming a model the run will not use is worse
// than no label. An uncurated stored name (config.json is hand-editable) is not
// usable as a *review* model, so both fall through to the task model rather than one
// promising it and the other dropping it.
func (c *Config) EffectiveReviewModel() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	review := strings.TrimSpace(c.ReviewModel)
	task := strings.TrimSpace(c.Model)
	c.mu.RUnlock()
	if review != "" && grokrun.IsKnownModel(review) {
		return review
	}
	return task
}

// RequestedAgentCLI resolves a model chosen for a session that does not exist yet
// — the web start form and the review/PR dispatch pickers.
//
// Unlike ResolveAgentCLI, the *model* leads: the agent is whoever owns the name,
// because a caller asking for "claude-opus-5" is asking for claude. Only curated
// names are accepted, so a forged form value can never reach the CLI. Empty means
// "no preference" and falls back to global config.
func (c *Config) RequestedAgentCLI(model string) (AgentCLI, error) {
	m := strings.TrimSpace(model)
	if m == "" {
		return c.ResolveAgentCLI(""), nil
	}
	if !grokrun.IsKnownModel(m) {
		return AgentCLI{}, fmt.Errorf("model %q is not a known model", m)
	}
	a, ok := grokrun.AgentForModel(m)
	if !ok {
		// Curated names always identify an agent; treat a mismatch as a bug in the
		// option list rather than guessing which CLI to hand it to.
		return AgentCLI{}, fmt.Errorf("model %q does not identify a coding CLI", m)
	}
	if c == nil {
		return AgentCLI{Agent: a, Bin: a.DefaultBin(), Model: m}, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cliLocked(a, m), nil
}

// ReviewAgentCLI is RequestedAgentCLI for review sessions: an empty choice falls
// back to the configured review model before the task model.
//
// Reviews resolve their model at creation and stamp it, so a review always runs on
// the review model even though the session machinery would otherwise re-resolve
// against `model` at run start.
//
// Unlike `model`, an uncurated `reviewModel` is not passed through to the CLI: it
// falls back to the task default, which is also what EffectiveReviewModel reports,
// so the dispatch modal's "Default (…)" label always names the model that will run.
func (c *Config) ReviewAgentCLI(model string) (AgentCLI, error) {
	if m := strings.TrimSpace(model); m != "" {
		return c.RequestedAgentCLI(m)
	}
	// EffectiveReviewModel already dropped an uncurated stored name, so what is left
	// is either curated or the task model — and a hand-set task model may itself be
	// uncurated, which is legal there and reaches the CLI via ResolveAgentCLI.
	cli, err := c.RequestedAgentCLI(c.EffectiveReviewModel())
	if err != nil {
		return c.ResolveAgentCLI(""), nil
	}
	return cli, nil
}

// ResolveAgentCLI returns how to run the named agent. An empty or unrecognized
// name resolves to the default agent (see DefaultAgent).
func (c *Config) ResolveAgentCLI(agent string) AgentCLI {
	if c == nil {
		return AgentCLI{Agent: grokrun.AgentGrok, Bin: grokrun.AgentGrok.DefaultBin()}
	}
	requested, explicit := grokrun.ParseAgent(agent)
	explicit = explicit && strings.TrimSpace(agent) != ""

	c.mu.RLock()
	defer c.mu.RUnlock()
	a := requested
	if !explicit {
		a = c.defaultAgentLocked()
	}
	return c.cliLocked(a, c.Model)
}

// cliLocked builds the invocation for agent a, applying model only when a owns
// it. A model from the other vendor is dropped rather than passed through, which
// would make the CLI fail on a name it has never heard of.
func (c *Config) cliLocked(a grokrun.Agent, model string) AgentCLI {
	out := AgentCLI{Agent: a}
	switch a {
	case grokrun.AgentClaude:
		out.Bin = strings.TrimSpace(c.ClaudeBin)
		out.ExtraArgs = slices.Clone(c.ClaudeExtraArgs)
	default:
		out.Bin = strings.TrimSpace(c.GrokBin)
		out.ExtraArgs = slices.Clone(c.ExtraArgs)
	}
	if out.Bin == "" {
		out.Bin = a.DefaultBin()
	}
	if m := strings.TrimSpace(model); m != "" {
		if owner, ok := grokrun.AgentForModel(m); !ok || owner == a {
			// Unowned names are passed through so a custom or self-hosted model id
			// still reaches the CLI it was configured for.
			out.Model = m
		}
	}
	return out
}

// ResolveSummarizeCLI is the invocation for thread-title summarization.
//
// Naming a thread is a single tools-off turn returning a few words, so it can run
// on a cheaper model — and, unlike a task run, on a different CLI entirely: the
// title call's session id is discarded, so nothing about the thread depends on
// which agent produced it. A summarizeModel naming an agent therefore selects
// that agent, which does require its binary to be installed. Empty falls back to
// the task model.
func (c *Config) ResolveSummarizeCLI(agent string) AgentCLI {
	if c == nil {
		return AgentCLI{Agent: grokrun.AgentGrok, Bin: grokrun.AgentGrok.DefaultBin()}
	}
	// Deliberately not nested inside ResolveAgentCLI's RLock: RWMutex stops
	// admitting readers once a writer is queued, so a nested read lock on one
	// goroutine can deadlock against a concurrent config save from the web UI.
	c.mu.RLock()
	summarize := strings.TrimSpace(c.SummarizeModel)
	owner, owned := grokrun.AgentForModel(summarize)
	c.mu.RUnlock()

	if summarize == "" {
		return c.ResolveAgentCLI(agent)
	}
	if owned {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.cliLocked(owner, summarize)
	}
	// Unowned name: keep the session's agent and hand it the name as configured.
	out := c.ResolveAgentCLI(agent)
	out.Model = summarize
	return out
}

// PinnedAgentCLI is the invocation for a session that already stamped its agent
// and model at creation. Global config cannot alter it — only the binary and
// extra args come from config, since those are host paths rather than session
// state, so moving a binary still works without disturbing live threads.
//
// An empty agent is pre-agent data: those sessions ran on grok with whatever the
// global model was, so that is what they keep running on.
func (c *Config) PinnedAgentCLI(agent, model string) AgentCLI {
	if c == nil {
		return AgentCLI{Agent: grokrun.AgentGrok, Bin: grokrun.AgentGrok.DefaultBin()}
	}
	stamped := strings.TrimSpace(agent) != ""
	a, ok := grokrun.ParseAgent(agent)
	if !ok {
		a = grokrun.AgentGrok
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !stamped {
		return c.cliLocked(grokrun.AgentGrok, c.Model)
	}
	return c.cliLocked(a, model)
}

// ModelChoice is one option for the config page's model dropdowns.
type ModelChoice struct {
	Value string
	Label string
	// Selected marks the currently configured value.
	Selected bool
}

// ModelGroup is one optgroup: the models belonging to a single CLI. Grouping is
// computed here rather than in the template so the option list and its group
// label come from the same inference used at run time.
type ModelGroup struct {
	// Agent is the CLI these names route to.
	Agent   string
	Label   string
	Choices []ModelChoice
}

// ModelGroups returns the dropdown options grouped by agent, marking current as
// selected. When current is set but not one of the curated options it is appended
// to its inferred agent's group, so a value written into config.json by hand is
// offered rather than silently dropped the next time someone saves the form.
func ModelGroups(current string) []ModelGroup {
	current = strings.TrimSpace(current)
	groups := []ModelGroup{
		{Agent: grokrun.AgentGrok.String(), Label: grokrun.AgentGrok.Label()},
		{Agent: grokrun.AgentClaude.String(), Label: grokrun.AgentClaude.Label()},
	}
	add := func(agent grokrun.Agent, choice ModelChoice) {
		name := agent.String()
		for i := range groups {
			if groups[i].Agent == name {
				groups[i].Choices = append(groups[i].Choices, choice)
				return
			}
		}
	}
	for _, opt := range grokrun.ModelOptions() {
		add(opt.Agent, ModelChoice{Value: opt.Value, Label: opt.Label, Selected: opt.Value == current})
	}
	if current != "" && !grokrun.IsKnownModel(current) {
		agent, known := grokrun.AgentForModel(current)
		label := current + " (from config)"
		if !known {
			label = current + " (from config — agent not identified)"
		}
		add(agent, ModelChoice{Value: current, Label: label, Selected: true})
	}
	// Drop empty groups so the select has no stray labels.
	out := make([]ModelGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.Choices) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// validModelChoice reports whether a submitted model may be persisted. Empty is
// always allowed (means "let the CLI pick"); otherwise it must be curated or the
// value already stored, so the form can never introduce an unvetted name but also
// never destroys one an operator set by hand.
func validModelChoice(submitted, current string) bool {
	submitted = strings.TrimSpace(submitted)
	return submitted == "" ||
		grokrun.IsKnownModel(submitted) ||
		submitted == strings.TrimSpace(current)
}

// AgentSettings is the editable agent configuration written by the web UI.
// Model fields hold a name from either vendor; empty means "let the CLI pick".
type AgentSettings struct {
	// Agent is the fallback used when Model does not identify one.
	Agent string
	Model string
	// SummarizeModel applies to thread-title summarization only. Empty falls back
	// to Model.
	SummarizeModel string
	// ReviewModel is the default for review sessions started from the web. Empty
	// falls back to Model.
	ReviewModel         string
	GrokBin             string
	ClaudeBin           string
	IncludeAnthropicEnv bool
}

// SetAgentSettings persists the agent defaults edited from the web UI.
// Existing sessions keep the agent they were stamped with; only new sessions
// pick up a changed default.
func (c *Config) SetAgentSettings(in AgentSettings) error {
	name, ok := grokrun.ParseAgent(in.Agent)
	if !ok {
		return fmt.Errorf("agent %q is not a known coding CLI (want %q or %q)",
			in.Agent, grokrun.AgentGrok, grokrun.AgentClaude)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validModelChoice(in.Model, c.Model) {
		return fmt.Errorf("model %q is not a known model", in.Model)
	}
	if !validModelChoice(in.SummarizeModel, c.SummarizeModel) {
		return fmt.Errorf("thread-title model %q is not a known model", in.SummarizeModel)
	}
	if !validModelChoice(in.ReviewModel, c.ReviewModel) {
		return fmt.Errorf("review model %q is not a known model", in.ReviewModel)
	}
	c.Agent = name.String()
	c.Model = strings.TrimSpace(in.Model)
	c.SummarizeModel = strings.TrimSpace(in.SummarizeModel)
	c.ReviewModel = strings.TrimSpace(in.ReviewModel)
	c.GrokBin = binOrDefault(in.GrokBin, grokrun.AgentGrok)
	c.ClaudeBin = binOrDefault(in.ClaudeBin, grokrun.AgentClaude)
	c.ClaudeIncludeAnthropicEnv = &in.IncludeAnthropicEnv
	return c.saveLocked()
}

func binOrDefault(bin string, a grokrun.Agent) string {
	if b := strings.TrimSpace(bin); b != "" {
		return b
	}
	return a.DefaultBin()
}

// AgentIncludesAnthropicEnv reports whether ANTHROPIC_* survives the denylist
// for this agent. Only claude can use those variables.
//
// ANTHROPIC_* is on the built-in denylist so host credentials never reach an
// agent child. claude does not need it — an OAuth/keychain login authenticates
// without any env var — so the passthrough stays opt-in for API-key, gateway,
// or custom base-URL setups, mirroring how GitHub tokens are gated.
func (c *Config) AgentIncludesAnthropicEnv(agent grokrun.Agent) bool {
	if c == nil || agent.Resolve() != grokrun.AgentClaude {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ClaudeIncludeAnthropicEnv != nil && *c.ClaudeIncludeAnthropicEnv
}
