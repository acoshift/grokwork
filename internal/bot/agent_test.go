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

// An investigate run's tool allowlist is agent-specific vocabulary, so it has to
// follow the agent the *message* chose, not the one the session will be stamped
// with after the run. Reading it back from the session made the first run of
// "@Grok /claude /investigate …" hand claude grok's tool names — an allowlist of
// zero real tools, i.e. a confidently empty investigation.
func TestFirstRunInvestigateToolsFollowRequestedAgent(t *testing.T) {
	cfg := &config.Config{Agent: "grok", Model: "grok-4.5", GrokBin: "grok", ClaudeBin: "claude"}
	b, _ := pinBot(t, cfg)
	item := taskItem{
		parsed:   Parsed{Kind: KindStartInvestigate, Agent: "claude", Prompt: "look around"},
		threadID: "fresh",
		proj:     projectRef{Name: "app", Cwd: "/tmp"},
		actor:    Actor{ID: "u1", DisplayName: "U"},
	}
	cli := b.threadCLI("fresh", item.parsed.Agent)
	if cli.Agent != grokrun.AgentClaude {
		t.Fatalf("agent=%q, want claude from the request", cli.Agent)
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
	got := b.threadCLI("t1", "")
	if got.Agent != grokrun.AgentClaude || got.Model != "sonnet" {
		t.Fatalf("cli=%+v, want the pinned claude/sonnet", got)
	}

	// Repoint global config entirely; the thread must not move.
	cfg.Model = "grok-4.5"
	cfg.Agent = "grok"
	if got := b.threadCLI("t1", ""); got.Agent != grokrun.AgentClaude || got.Model != "sonnet" {
		t.Fatalf("config change leaked into a live session: %+v", got)
	}
	// Nor may an explicit request on a later message move it.
	if got := b.threadCLI("t1", "grok"); got.Agent != grokrun.AgentClaude {
		t.Fatalf("a later /grok moved a pinned session: %+v", got)
	}
	if _, pinned := b.pinnedAgent("t1"); !pinned {
		t.Fatal("session with a stamp must report as pinned")
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
	got := b.threadCLI("t2", "")
	if got.Agent != grokrun.AgentGrok || got.Model != "grok-4.5" {
		t.Fatalf("cli=%+v, want grok with the global model", got)
	}
	if _, pinned := b.pinnedAgent("t2"); !pinned {
		t.Fatal("a session that already ran is pinned even without a stamp")
	}
}

// Before the first run there is nothing to protect, so config and an explicit
// request still decide.
func TestThreadCLIUnstartedSessionStillChoosable(t *testing.T) {
	cfg := &config.Config{Agent: "grok", Model: "grok-4.5", GrokBin: "grok", ClaudeBin: "claude"}
	b, store := pinBot(t, cfg)
	// An entry with no session id yet (e.g. a case opened before any run).
	if err := store.Set("t3", sessionstore.Entry{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, pinned := b.pinnedAgent("t3"); pinned {
		t.Fatal("a session that has not run must not be pinned")
	}
	if got := b.threadCLI("t3", "claude"); got.Agent != grokrun.AgentClaude {
		t.Fatalf("cli=%+v, want the requested claude", got)
	}
	if got := b.threadCLI("t3", ""); got.Agent != grokrun.AgentGrok {
		t.Fatalf("cli=%+v, want the configured default", got)
	}

	// Stamping before the first run fixes it from then on.
	b.stampSessionCLI("t3", cfg.ResolveAgentCLI("claude"))
	if _, pinned := b.pinnedAgent("t3"); !pinned {
		t.Fatal("stamped session should be pinned")
	}
	if got := b.threadCLI("t3", "grok"); got.Agent != grokrun.AgentClaude {
		t.Fatalf("stamp not honored: %+v", got)
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
	if got := b.threadSummarizeCLI("c1", ""); got.Agent != grokrun.AgentClaude || got.Model != "haiku" {
		t.Fatalf("cli=%+v, want claude/haiku", got)
	}
	// …and the run itself is untouched by the title model.
	if got := b.threadCLI("c1", ""); got.Model != "sonnet" {
		t.Fatalf("run cli=%+v, want the pinned sonnet", got)
	}

	// Grok thread + claude title model: the title crosses to claude. This is the
	// common shape — expensive tasks on one CLI, titles on the cheapest model
	// available anywhere.
	if err := store.Set("g1", sessionstore.Entry{SessionID: "s", Agent: "grok", Model: "grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	if got := b.threadSummarizeCLI("g1", ""); got.Agent != grokrun.AgentClaude || got.Model != "haiku" {
		t.Fatalf("cli=%+v, want claude/haiku for the title", got)
	}
	if got := b.threadCLI("g1", ""); got.Agent != grokrun.AgentGrok || got.Model != "grok-4.5" {
		t.Fatalf("run cli=%+v, want the pinned grok/grok-4.5", got)
	}

	// No title model configured: the title uses the thread's own agent.
	cfg.SummarizeModel = ""
	if got := b.threadSummarizeCLI("g1", ""); got.Agent != grokrun.AgentGrok {
		t.Fatalf("cli=%+v, want grok when no title model is set", got)
	}
}

func TestParseAgentCommandBareForms(t *testing.T) {
	cases := map[string]string{
		"/agent":  "",
		"/claude": "claude",
		"/grok":   "grok",
	}
	for in, wantArg := range cases {
		got := ParseMessage(in, "")
		if got.Kind != KindAgent {
			t.Errorf("ParseMessage(%q).Kind=%d want KindAgent", in, got.Kind)
		}
		if got.Arg != wantArg {
			t.Errorf("ParseMessage(%q).Arg=%q want %q", in, got.Arg, wantArg)
		}
	}
}

func TestParseAgentCommandSetOnly(t *testing.T) {
	got := ParseMessage("/agent claude", "")
	if got.Kind != KindAgent || got.Arg != "claude" {
		t.Fatalf("got kind=%d arg=%q", got.Kind, got.Arg)
	}
}

// An unknown name must not fall through and silently run on the default agent.
func TestParseAgentCommandUnknownNameDropsTask(t *testing.T) {
	got := ParseMessage("/agent gpt do the thing", "")
	if got.Kind != KindAgent || got.Arg != "gpt" {
		t.Fatalf("got kind=%d arg=%q", got.Kind, got.Arg)
	}
	if got.Agent != "" {
		t.Fatalf("unknown agent leaked into the run: %q", got.Agent)
	}
}

func TestParseAgentCommandWithTask(t *testing.T) {
	for _, in := range []string{"/agent claude fix the flaky test", "/claude fix the flaky test"} {
		got := ParseMessage(in, "")
		if got.Kind != KindTask {
			t.Errorf("ParseMessage(%q).Kind=%d want KindTask", in, got.Kind)
		}
		if got.Agent != "claude" {
			t.Errorf("ParseMessage(%q).Agent=%q", in, got.Agent)
		}
		if got.Prompt != "fix the flaky test" {
			t.Errorf("ParseMessage(%q).Prompt=%q", in, got.Prompt)
		}
	}
}

// Agent selection is orthogonal to mode, so it composes with other commands.
func TestParseAgentCommandComposesWithStart(t *testing.T) {
	got := ParseMessage("/claude /start investigate why is CI red", "")
	if got.Kind != KindStartInvestigate {
		t.Fatalf("kind=%d want KindStartInvestigate", got.Kind)
	}
	if got.Agent != "claude" || got.Prompt != "why is CI red" {
		t.Fatalf("agent=%q prompt=%q", got.Agent, got.Prompt)
	}
}

// Free-form prose that merely mentions an agent stays a task.
func TestParseAgentCommandRequiresSlash(t *testing.T) {
	for _, in := range []string{"claude should look at this", "grok the logs for me", "agent claude"} {
		got := ParseMessage(in, "")
		if got.Kind != KindTask {
			t.Errorf("ParseMessage(%q).Kind=%d want KindTask", in, got.Kind)
		}
		if got.Agent != "" {
			t.Errorf("ParseMessage(%q) set Agent=%q", in, got.Agent)
		}
	}
}

func TestParseAgentCommandPreservesSpecialChars(t *testing.T) {
	got := ParseMessage("/claude fix #42 at https://ex.com/a?x=1&b=2", "")
	if got.Prompt != "fix #42 at https://ex.com/a?x=1&b=2" {
		t.Fatalf("prompt=%q", got.Prompt)
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
func TestBuildRunPolicyInvestigateToolsFollowAgent(t *testing.T) {
	base := PolicyInput{ForceInvestigate: true}

	grokPol := BuildRunPolicy(base)
	if grokPol.Tools == nil || *grokPol.Tools != "read_file,grep" {
		t.Fatalf("grok tools=%v", grokPol.Tools)
	}

	base.Agent = grokrun.AgentClaude
	claudePol := BuildRunPolicy(base)
	if claudePol.Tools == nil || *claudePol.Tools != "Read,Grep,Glob" {
		t.Fatalf("claude tools=%v", claudePol.Tools)
	}
}

// A project override is written in one agent's vocabulary. Passing grok names to
// claude yields an allowlist of zero real tools, so the override is only applied
// to the agent it was written for.
func TestBuildRunPolicyIgnoresForeignToolOverride(t *testing.T) {
	in := PolicyInput{ForceInvestigate: true, InvestigateTools: "read_file,list_dir"}

	if pol := BuildRunPolicy(in); pol.Tools == nil || *pol.Tools != "read_file,list_dir" {
		t.Fatalf("grok should honor its own override: %v", pol.Tools)
	}

	in.Agent = grokrun.AgentClaude
	pol := BuildRunPolicy(in)
	if pol.Tools == nil || *pol.Tools != "Read,Grep,Glob" {
		t.Fatalf("claude should fall back to its own default, got %v", pol.Tools)
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
