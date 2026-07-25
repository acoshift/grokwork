package bot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/runjournal"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// A session's agent and model are pinned at creation and never change. Session
// ids are not portable between CLIs, and a thread whose model shifts mid-way
// answers inconsistently — so global config only decides what a *new* session
// starts on. Pick per thread on the message that opens it: "@Grok /claude <task>".

// threadCLI returns the CLI for a run on this thread.
func (b *Bot) threadCLI(threadID, requested string) config.AgentCLI {
	e, exists := b.session(threadID)
	switch {
	case exists && strings.TrimSpace(e.Agent) != "":
		// Pinned at creation.
		return b.cfg.PinnedAgentCLI(e.Agent, e.Model)
	case exists && strings.TrimSpace(e.SessionID) != "":
		// Ran before agents were selectable: grok, with the global model.
		return b.cfg.PinnedAgentCLI("", "")
	default:
		// No run yet, so this message still gets to choose.
		return b.cfg.ResolveAgentCLI(requested)
	}
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
func (b *Bot) threadSummarizeCLI(threadID, requested string) config.AgentCLI {
	run := b.threadCLI(threadID, requested)
	return b.cfg.ResolveSummarizeCLI(run.Agent.String())
}

// pinnedAgent reports the agent a thread is locked to, and whether it is locked
// at all. Unlocked means no run has happened yet.
func (b *Bot) pinnedAgent(threadID string) (grokrun.Agent, bool) {
	e, ok := b.session(threadID)
	if !ok {
		return b.cfg.DefaultAgent(), false
	}
	if a, parsed := grokrun.ParseAgent(e.Agent); parsed && strings.TrimSpace(e.Agent) != "" {
		return a, true
	}
	if strings.TrimSpace(e.SessionID) != "" {
		// Pre-agent session: locked to grok by history.
		return grokrun.AgentGrok, true
	}
	return b.cfg.DefaultAgent(), false
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
// before a session has an id, so nothing is invalidated; once the session has run
// these values are what threadCLI reads and config changes no longer reach it.
func (b *Bot) stampSessionCLI(threadID string, cli config.AgentCLI) {
	_, _, err := b.sessions.Patch(threadID, func(e *sessionstore.Entry) {
		e.Agent = cli.Agent.String()
		e.Model = cli.Model
	})
	if err != nil {
		log.Printf("warn: stamp session cli thread=%s: %v", threadID, err)
	}
}

// ensureSessionCLI pins the agent and model on a session that has not stamped
// them yet, and leaves an already-stamped session alone.
//
// This runs at run *start*, not run end, and that ordering is the whole point: a
// crash mid-first-run leaves a journal but no stamp, and the recovery re-drive
// carries no agent in its rehydrated Parsed — so an unstamped thread would be
// re-resolved from global config and a thread opened with /claude could come back
// as grok, permanently. Stamping first means recovery reads the choice instead of
// guessing it.
func (b *Bot) ensureSessionCLI(threadID string, cli config.AgentCLI) {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	if e, ok := b.session(threadID); ok && strings.TrimSpace(e.Agent) != "" {
		return
	}
	b.stampSessionCLI(threadID, cli)
}

// looksLikeAgentChild reports whether pid is still the agent child this journal
// started, using the agent recorded on the journal to pick the expected binary.
func (b *Bot) looksLikeAgentChild(pid int, journalAgent string) bool {
	return runjournal.LooksLikeAgentCLI(pid, journalAgent, b.cfg.ResolveAgentCLI(journalAgent).Bin)
}

// handleAgent: @Grok /agent [name], @Grok /claude, @Grok /grok
func (b *Bot) handleAgent(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	name := strings.TrimSpace(parsed.Arg)
	if name == "" {
		b.replyAgentStatus(s, m)
		return
	}
	agent, ok := grokrun.ParseAgent(name)
	if !ok {
		replyText(s, m, fmt.Sprintf("Unknown agent %q. Available: `grok`, `claude`.", name))
		return
	}

	if !isThread(s, m.ChannelID) {
		replyText(s, m, fmt.Sprintf(
			"Use `@Grok /agent %s <task>` to open a thread on **%s**. The agent is chosen when a session starts and stays fixed after that.",
			agent, agent.Label()))
		return
	}
	e, exists := b.session(m.ChannelID)
	if !exists {
		replyText(s, m, fmt.Sprintf("No session for this thread yet — send `@Grok /agent %s <task>` to start one on %s.", agent, agent.Label()))
		return
	}

	// Already ran: the agent and model are part of the session's identity.
	if current, pinned := b.pinnedAgent(m.ChannelID); pinned {
		if current.Resolve() == agent.Resolve() {
			replyText(s, m, fmt.Sprintf("This thread runs on **%s**.", agent.Label()))
			return
		}
		replyText(s, m, fmt.Sprintf(
			"This thread runs on **%s** and cannot be moved to %s — the %s session cannot be resumed by %s.\n_Start a new thread with `@Grok /%s <task>`._",
			current.Label(), agent.Label(), current.Label(), agent.Label(), agent))
		return
	}

	// Not yet run: still choosable, but it decides the session, so gate it like /reset.
	if !b.canControlThread(s, m, e) {
		b.denyControl(s, m, e, "choose agent")
		return
	}
	cli := b.cfg.ResolveAgentCLI(agent.String())
	b.stampSessionCLI(m.ChannelID, cli)
	msg := fmt.Sprintf("This thread will run on **%s**", cli.Agent.Label())
	if cli.Model != "" {
		msg += fmt.Sprintf(" (`%s`)", cli.Model)
	}
	replyText(s, m, msg+". Fixed once the first task runs.")
}

func (b *Bot) replyAgentStatus(s *discordgo.Session, m *discordgo.MessageCreate) {
	def := b.cfg.DefaultAgent()
	var lines []string
	if isThread(s, m.ChannelID) {
		cli := b.threadCLI(m.ChannelID, "")
		_, pinned := b.pinnedAgent(m.ChannelID)
		suffix := ""
		if pinned {
			suffix = " (fixed for this session)"
		}
		lines = append(lines, fmt.Sprintf("**Agent:** %s%s", cli.Agent.Label(), suffix))
		if cli.Model != "" {
			lines = append(lines, fmt.Sprintf("**Model:** `%s`", cli.Model))
		}
	}
	lines = append(lines,
		fmt.Sprintf("**Default for new threads:** %s", def.Label()),
		"",
		"`/claude <task>` · `/grok <task>` — open a thread on a specific agent",
		"_The agent and model are fixed when a session starts; start a new thread to use a different one._",
	)
	replyText(s, m, strings.Join(lines, "\n"))
}
