package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/runjournal"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Which CLI a session runs on is decided once, by global config, when its first
// run starts — Discord cannot choose or change it. The stamp is still required
// even though nobody picks: session ids are not portable between CLIs, so editing
// the global model must not reach a thread that is already running on the old one.

// threadCLI returns the CLI for a run on this thread.
func (b *Bot) threadCLI(threadID string) config.AgentCLI {
	e, exists := b.session(threadID)
	switch {
	case exists && strings.TrimSpace(e.Agent) != "":
		// Stamped on its first run; config changes no longer reach it.
		return b.cfg.PinnedAgentCLI(e.Agent, e.Model)
	case exists && strings.TrimSpace(e.SessionID) != "":
		// Ran before the agent was recorded: grok, with the global model.
		return b.cfg.PinnedAgentCLI("", "")
	default:
		// No run yet: whatever config currently says.
		return b.cfg.ResolveAgentCLI("")
	}
}

// ThreadAgent names the CLI this thread's runs use, for surfaces that only need
// the name — the web session page labels reply bubbles and the composer with it.
// It resolves through threadCLI so the label can never disagree with what runs:
// a thread stamped on claude must not have its history captioned "Grok".
func (b *Bot) ThreadAgent(threadID string) grokrun.Agent {
	if b == nil || b.cfg == nil {
		return grokrun.AgentGrok
	}
	return b.threadCLI(threadID).Agent.Resolve()
}

// threadSummarizeCLI is threadCLI for thread-title summarization.
//
// Naming a thread is one tools-off turn whose session id is discarded, so it is
// not part of the session and follows the configured title model even on a pinned
// thread — including when that model belongs to the *other* CLI. Crossing CLIs is
// the whole point of the setting: it is what lets an expensive thread get a cheap
// title. If that CLI is missing or unauthenticated the call just fails and
// improveThreadTitle keeps the locally derived title, so the downside is one
// wasted call rather than a broken thread.
func (b *Bot) threadSummarizeCLI(threadID string) config.AgentCLI {
	run := b.threadCLI(threadID)
	return b.cfg.ResolveSummarizeCLI(run.Agent.String())
}

// session reads a session entry, tolerating the nil store used in unit tests.
func (b *Bot) session(threadID string) (sessionstore.Entry, bool) {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return sessionstore.Entry{}, false
	}
	return b.sessions.Get(threadID)
}

// childEnvPolicy builds the Layer A environment gate for one run.
func (b *Bot) childEnvPolicy(agent grokrun.Agent, pol RunPolicy) grokrun.ChildEnvPolicy {
	return grokrun.ChildEnvPolicy{
		IncludeGHToken:      pol.IncludeGHToken,
		IncludeAnthropicEnv: b.cfg.AgentIncludesAnthropicEnv(agent),
		ExtraDenylist:       b.cfg.GrokEnvDenylistPrefixes(),
	}
}

// childEnvPolicyWithAgentToken is childEnvPolicy plus optional agent token re-admit.
func (b *Bot) childEnvPolicyWithAgentToken(agent grokrun.Agent, pol RunPolicy, includeToken bool) grokrun.ChildEnvPolicy {
	p := b.childEnvPolicy(agent, pol)
	p.IncludeAgentToken = includeToken
	return p
}

// sameAgent reports whether a stored agent name refers to agent. An empty stored
// name is pre-agent data, which was always grok.
func sameAgent(stored string, agent grokrun.Agent) bool {
	parsed, ok := grokrun.ParseAgent(stored)
	if !ok {
		return false
	}
	return parsed.Resolve() == agent.Resolve()
}

// stampSessionCLI records the agent and model a session is created with. Called
// before the session has an id, so nothing is invalidated; from then on these are
// what threadCLI reads and global config no longer reaches the thread.
//
// A missing entry is reported rather than swallowed: Patch is a silent no-op that
// returns a nil error when the thread is unknown, and "the pin landed" is the one
// thing this function exists to guarantee.
func (b *Bot) stampSessionCLI(threadID string, cli config.AgentCLI) error {
	_, ok, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		e.Agent = cli.Agent.String()
		e.Model = cli.Model
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("stamp agent/model: unknown session %q", threadID)
	}
	return nil
}

// ensureSessionCLI pins the agent and model on a session that has not stamped
// them yet, and leaves an already-stamped session alone.
//
// This runs at run *start*, not run end, and that ordering is load-bearing.
// Config is editable while the bot runs, so the global model can change between a
// run starting and its recovery re-drive; an unstamped thread would then be
// resolved against the *new* config and try to resume the old CLI's session id.
// Stamping first means recovery reads what the run actually used.
func (b *Bot) ensureSessionCLI(threadID string, cli config.AgentCLI) {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	if e, ok := b.session(threadID); ok && strings.TrimSpace(e.Agent) != "" {
		return
	}
	// Best-effort at run start: the run proceeds either way, and threadCLI already
	// resolved the CLI it will use.
	if err := b.stampSessionCLI(threadID, cli); err != nil {
		log.Printf("warn: stamp session cli thread=%s: %v", threadID, err)
	}
}

// A model chosen in the web UI reaches a run by being stamped on the session at
// creation — the same stamp threadCLI already reads. Nothing else in the run path
// learns about "requested" models, and the stamp is only ever written while the
// session is new, so reusing a session can never re-point it at another CLI.

// requireCanSelectModel fails when the actor lacks builder-class caps. Only
// consult it when a model was actually named: an empty choice is the configured
// default and is available to anyone who may start a session at all.
// Fails closed without config, unlike requireCanStartFix: there is no legacy data
// to be lenient about here, since nothing could have named a model before this
// existed.
func (b *Bot) requireCanSelectModel(project, userID string) error {
	if b == nil || b.cfg == nil {
		return ErrCannotSelectModel
	}
	if !b.cfg.ResolveCapabilities(project, userID).CanShip() {
		return ErrCannotSelectModel
	}
	return nil
}

// stampNewSessionCLI pins agent+model on a session being created.
//
// Callers must invoke this before the first run is enqueued: executeTask stamps
// the *config* default at run start (ensureSessionCLI), and whoever stamps first
// wins. Stamping late would leave the run on the global model while the session
// claimed the requested one. The error is returned rather than logged — a create
// path that cannot pin the model must fail, not run on the wrong one.
func (b *Bot) stampNewSessionCLI(threadID string, cli config.AgentCLI) error {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return nil
	}
	return b.stampSessionCLI(threadID, cli)
}

// looksLikeAgentChild reports whether pid is still the agent child this journal
// started, using the agent recorded on the journal to pick the expected binary.
func (b *Bot) looksLikeAgentChild(pid int, journalAgent string) bool {
	return runjournal.LooksLikeAgentCLI(pid, journalAgent, b.cfg.ResolveAgentCLI(journalAgent).Bin)
}
