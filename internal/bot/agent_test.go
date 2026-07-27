package bot

import (
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// pinBot builds a Bot over a real session store with the given global config.
func pinBot(t *testing.T, cfg *config.Config) (*Bot, *sessionstore.Store) {
	t.Helper()
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	return New(cfg, store, nil), store
}

// An investigate run's tool allowlist is agent-specific vocabulary, so it must
// follow the agent the run actually uses. Reading it back from the session instead
// handed claude grok's tool names on a thread's first run — an allowlist of zero
// real tools, i.e. a confidently empty investigation.
func TestFirstRunInvestigateToolsFollowResolvedAgent(t *testing.T) {
	cfg := &config.Config{Agent: "claude", GrokBin: "grok", ClaudeBin: "claude"}
	b, _ := pinBot(t, cfg)
	item := taskItem{
		parsed:   Parsed{Kind: KindStartInvestigate, Prompt: "look around"},
		threadID: "fresh",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u1", DisplayName: "U"},
	}
	cli := b.threadCLI("fresh")
	if cli.Agent != grokrun.AgentClaude {
		t.Fatalf("agent=%q, want claude from config", cli.Agent)
	}
	pol := b.resolveRunPolicy("fresh", "app", item, "pr", item.actor, cli.Agent)
	if pol.Tools == nil {
		t.Fatal("investigate must set a tools allowlist")
	}
	want := grokrun.AgentClaude.DefaultInvestigateTools()
	if *pol.Tools != want {
		t.Fatalf("tools=%q, want claude vocabulary %q", *pol.Tools, want)
	}
}

// A session pins its agent and model at creation. Later config edits must not
// reach it: its session id belongs to that CLI, and a mid-thread model change
// makes the thread answer inconsistently.
func TestThreadCLIPinnedAgainstConfigChanges(t *testing.T) {
	cfg := &config.Config{Agent: "grok", Model: "grok-4.5", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)

	if err := store.Set("t1", sessionstore.Entry{
		SessionID: "sess-1", Agent: "claude", Model: "sonnet",
	}); err != nil {
		t.Fatal(err)
	}
	got := b.threadCLI("t1")
	if got.Agent != grokrun.AgentClaude || got.Model != "sonnet" {
		t.Fatalf("cli=%+v, want the pinned claude/sonnet", got)
	}

	// Repoint global config entirely; the thread must not move.
	cfg.Model = "grok-4.5"
	cfg.Agent = "grok"
	if got := b.threadCLI("t1"); got.Agent != grokrun.AgentClaude || got.Model != "sonnet" {
		t.Fatalf("config change leaked into a live session: %+v", got)
	}
}

// Sessions created before agents were selectable carry no stamp. They ran on grok
// with whatever the global model was, and must keep doing exactly that.
func TestThreadCLILegacySessionStaysGrok(t *testing.T) {
	cfg := &config.Config{Agent: "claude", Model: "grok-4.5", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)
	if err := store.Set("t2", sessionstore.Entry{SessionID: "sess-legacy"}); err != nil {
		t.Fatal(err)
	}
	got := b.threadCLI("t2")
	if got.Agent != grokrun.AgentGrok || got.Model != "grok-4.5" {
		t.Fatalf("cli=%+v, want grok with the global model", got)
	}
	// Even with claude configured globally: an unstamped session that has already
	// run is grok by history, and its transcript is not portable.
	cfg.Model = "sonnet"
	if got := b.threadCLI("t2"); got.Agent != grokrun.AgentGrok {
		t.Fatalf("cli=%+v, a pre-existing session must not be moved by config", got)
	}
}

// Before the first run there is nothing to protect, so global config decides —
// and once stamped, it does not.
func TestThreadCLIUnstartedSessionFollowsConfig(t *testing.T) {
	cfg := &config.Config{Agent: "grok", Model: "grok-4.5", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)
	// An entry with no session id yet (e.g. a case opened before any run).
	if err := store.Set("t3", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if got := b.threadCLI("t3"); got.Agent != grokrun.AgentGrok {
		t.Fatalf("cli=%+v, want the configured grok", got)
	}
	cfg.Agent = "claude"
	cfg.Model = ""
	if got := b.threadCLI("t3"); got.Agent != grokrun.AgentClaude {
		t.Fatalf("cli=%+v, want config to still decide before the first run", got)
	}

	// Stamping fixes it from then on, even against a further config change.
	b.ensureSessionCLI("t3", cfg.ResolveAgentCLI(""))
	cfg.Agent = "grok"
	if got := b.threadCLI("t3"); got.Agent != grokrun.AgentClaude {
		t.Fatalf("stamp not honored: %+v", got)
	}
}

// ensureSessionCLI must never move an already-stamped thread, whatever config now
// says — the thread's session id belongs to the CLI it was created on.
func TestEnsureSessionCLILeavesStampedThreadAlone(t *testing.T) {
	cfg := &config.Config{Agent: "grok", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)
	if err := store.Set("t4", sessionstore.Entry{SessionID: "s", Agent: "claude", Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	b.ensureSessionCLI("t4", cfg.ResolveAgentCLI(""))
	e, _ := store.Get("t4")
	if e.Agent != "claude" || e.Model != "sonnet" {
		t.Fatalf("stamp overwritten: agent=%q model=%q", e.Agent, e.Model)
	}
}

// The point of a separate title model is a cheap model for a throwaway turn, so
// it must apply even when the thread itself runs on the other CLI — a title's
// session id is discarded, so nothing about the thread depends on who wrote it.
// The task model stays pinned either way.
func TestThreadSummarizeCLICrossesAgentsForCheapTitles(t *testing.T) {
	cfg := &config.Config{Agent: "grok", Model: "grok-4.5", SummarizeModel: "haiku", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)

	// Claude thread + claude title model: the title model applies.
	if err := store.Set("c1", sessionstore.Entry{SessionID: "s", Agent: "claude", Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if got := b.threadSummarizeCLI("c1"); got.Agent != grokrun.AgentClaude || got.Model != "haiku" {
		t.Fatalf("cli=%+v, want claude/haiku", got)
	}
	// …and the run itself is untouched by the title model.
	if got := b.threadCLI("c1"); got.Model != "sonnet" {
		t.Fatalf("run cli=%+v, want the pinned sonnet", got)
	}

	// Grok thread + claude title model: the title crosses to claude. This is the
	// common shape — expensive tasks on one CLI, titles on the cheapest model
	// available anywhere.
	if err := store.Set("g1", sessionstore.Entry{SessionID: "s", Agent: "grok", Model: "grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	if got := b.threadSummarizeCLI("g1"); got.Agent != grokrun.AgentClaude || got.Model != "haiku" {
		t.Fatalf("cli=%+v, want claude/haiku for the title", got)
	}
	if got := b.threadCLI("g1"); got.Agent != grokrun.AgentGrok || got.Model != "grok-4.5" {
		t.Fatalf("run cli=%+v, want the pinned grok/grok-4.5", got)
	}

	// No title model configured: the title uses the thread's own agent.
	cfg.SummarizeModel = ""
	if got := b.threadSummarizeCLI("g1"); got.Agent != grokrun.AgentGrok {
		t.Fatalf("cli=%+v, want grok when no title model is set", got)
	}
}

// The agent/model selectors were removed from Discord: global config is the only
// source. These forms must therefore be ordinary tasks, with the words reaching
// the model verbatim rather than being eaten as commands.
func TestAgentSelectorsAreNoLongerCommands(t *testing.T) {
	for _, in := range []string{
		"/claude", "/grok", "/agent",
		"/agent claude", "/claude fix the flaky test", "/grok why is CI red",
		"claude should look at this",
	} {
		got := ParseMessage(in, "")
		if got.Kind != KindTask {
			t.Errorf("ParseMessage(%q).Kind=%d want KindTask", in, got.Kind)
		}
		if got.Prompt != in {
			t.Errorf("ParseMessage(%q).Prompt=%q want the text verbatim", in, got.Prompt)
		}
	}
}

func TestSameAgentTreatsEmptyAsGrok(t *testing.T) {
	// Sessions written before agents existed have no stamp and were always grok.
	if !sameAgent("", grokrun.AgentGrok) {
		t.Error("legacy session should resume on grok")
	}
	if sameAgent("", grokrun.AgentClaude) {
		t.Error("legacy grok session must not resume on claude")
	}
	if !sameAgent("claude", grokrun.AgentClaude) {
		t.Error("claude session should resume on claude")
	}
	if sameAgent("claude", grokrun.AgentGrok) {
		t.Error("claude session must not resume on grok")
	}
}

// The investigate allowlist must match the agent that will run, otherwise the
// model is handed tool names it does not have and cannot read the repo.
// Shell is role-gated: Investigate (or SafeOps/CanShip) grants diagnostic shell.
func TestBuildRunPolicyInvestigateToolsFollowAgent(t *testing.T) {
	// Zero caps → file-only for both agents.
	base := PolicyInput{ForceInvestigate: true}

	grokPol := BuildRunPolicy(base)
	if grokPol.Tools == nil || *grokPol.Tools != "read_file,grep" {
		t.Fatalf("grok file-only tools=%v", grokPol.Tools)
	}
	if grokPol.InvestigateShell {
		t.Fatal("zero caps must not grant investigate shell")
	}

	base.Agent = grokrun.AgentClaude
	claudePol := BuildRunPolicy(base)
	if claudePol.Tools == nil || *claudePol.Tools != "Read,Grep,Glob" {
		t.Fatalf("claude file-only tools=%v", claudePol.Tools)
	}

	// Investigator gets shell for diagnostics (psql, …) on both agents.
	base = PolicyInput{
		ForceInvestigate: true,
		Caps:             config.BuiltinCapabilityTemplates["investigator"],
	}
	if pol := BuildRunPolicy(base); pol.Tools == nil || *pol.Tools != "read_file,grep,run_terminal_command" || !pol.InvestigateShell {
		t.Fatalf("investigator grok tools=%v shell=%v", pol.Tools, pol.InvestigateShell)
	}
	base.Agent = grokrun.AgentClaude
	if pol := BuildRunPolicy(base); pol.Tools == nil || *pol.Tools != "Read,Grep,Glob,Bash" || !pol.InvestigateShell {
		t.Fatalf("investigator claude tools=%v shell=%v", pol.Tools, pol.InvestigateShell)
	}

	// Builder-class gets shell on both agents.
	base = PolicyInput{
		ForceInvestigate: true,
		Caps:             config.BuiltinCapabilityTemplates["builder"],
	}
	if pol := BuildRunPolicy(base); pol.Tools == nil || *pol.Tools != "read_file,grep,run_terminal_command" || !pol.InvestigateShell {
		t.Fatalf("builder grok tools=%v shell=%v", pol.Tools, pol.InvestigateShell)
	}
}

// A project override is written in one agent's vocabulary and only applies when
// shell is role-allowed. Passing grok names to claude yields zero real tools, so
// the override is only applied to the agent it was written for.
func TestBuildRunPolicyIgnoresForeignToolOverride(t *testing.T) {
	// Zero caps (no shell): override must not re-open shell via project config.
	in := PolicyInput{
		ForceInvestigate: true,
		InvestigateTools: "read_file,grep,run_terminal_command",
	}
	if pol := BuildRunPolicy(in); pol.Tools == nil || *pol.Tools != "read_file,grep" || pol.InvestigateShell {
		t.Fatalf("zero caps must stay file-only despite override: tools=%v shell=%v", pol.Tools, pol.InvestigateShell)
	}

	// Investigator: grok honors override; claude keeps its own shell default.
	in = PolicyInput{
		ForceInvestigate: true,
		InvestigateTools: "read_file,list_dir",
		Caps:             config.BuiltinCapabilityTemplates["investigator"],
	}
	if pol := BuildRunPolicy(in); pol.Tools == nil || *pol.Tools != "read_file,list_dir" {
		t.Fatalf("grok should honor its own override when shell allowed: %v", pol.Tools)
	}

	in.Agent = grokrun.AgentClaude
	pol := BuildRunPolicy(in)
	if pol.Tools == nil || *pol.Tools != "Read,Grep,Glob,Bash" {
		t.Fatalf("claude should fall back to its own shell default, got %v", pol.Tools)
	}
}

// Explain mode is tools-off for every agent (the "" pointer, rewritten per driver).
func TestBuildRunPolicyExplainStaysToolsOff(t *testing.T) {
	for _, agent := range []grokrun.Agent{grokrun.AgentGrok, grokrun.AgentClaude} {
		pol := BuildRunPolicy(PolicyInput{RequestedMode: ModeExplain, Agent: agent})
		if pol.Tools == nil || *pol.Tools != "" {
			t.Fatalf("agent=%s tools=%v want tools-off", agent, pol.Tools)
		}
	}
}
