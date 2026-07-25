package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/grokrun"
)

// loadAgentConfig writes a minimal config with the given top-level extras and loads it.
func loadAgentConfig(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw := `{
		"discordToken": "tok",
		"projects": { "p": "` + proj + `" },
		"channels": { "c1": "p" }` + extra + `
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_WORK_CONFIG", cfgPath)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("GROK_WORK_HTTP_LISTEN", "")
	return Load()
}

func TestAgentDefaultsToGrok(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultAgent(); got != grokrun.AgentGrok {
		t.Fatalf("default agent=%q", got)
	}
	// An unset claudeBin still resolves so /agent claude works without extra config.
	if got := cfg.ClaudeBin; got != "claude" {
		t.Fatalf("claudeBin=%q", got)
	}
}

// A typo must fail loudly: silently routing every session to grok would be very
// hard to notice from the outside.
func TestAgentUnknownNameRejected(t *testing.T) {
	if _, err := loadAgentConfig(t, `, "agent": "gpt"`); err == nil {
		t.Fatal("expected load to reject an unknown agent")
	}
	if _, err := loadAgentConfig(t, `, "agent": "claude"`); err != nil {
		t.Fatalf("claude should be accepted: %v", err)
	}
}

// The model name selects the CLI, so a Claude model routes to claude even when
// the fallback agent says grok.
func TestModelNamePicksTheAgent(t *testing.T) {
	cfg, err := loadAgentConfig(t, `,
		"agent": "grok",
		"model": "sonnet",
		"grokBin": "/usr/local/bin/grok",
		"claudeBin": "/opt/claude"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DefaultAgent(); got != grokrun.AgentClaude {
		t.Fatalf("default agent=%q, want claude from the model name", got)
	}
	got := cfg.ResolveAgentCLI("")
	if got.Agent != grokrun.AgentClaude || got.Bin != "/opt/claude" || got.Model != "sonnet" {
		t.Fatalf("cli=%+v", got)
	}
}

// Binaries and extra args stay per-agent: only the model is shared.
func TestResolveAgentCLIKeepsBinariesAndArgsSeparate(t *testing.T) {
	cfg, err := loadAgentConfig(t, `,
		"grokBin": "/usr/local/bin/grok",
		"extraArgs": ["--no-plan"],
		"claudeBin": "/opt/claude",
		"claudeExtraArgs": ["--effort", "high"]`)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.ResolveAgentCLI("grok")
	c := cfg.ResolveAgentCLI("claude")
	if g.Bin != "/usr/local/bin/grok" || c.Bin != "/opt/claude" {
		t.Fatalf("bins g=%q c=%q", g.Bin, c.Bin)
	}
	if len(g.ExtraArgs) != 1 || g.ExtraArgs[0] != "--no-plan" {
		t.Fatalf("grok extraArgs=%v", g.ExtraArgs)
	}
	if len(c.ExtraArgs) != 2 || c.ExtraArgs[0] != "--effort" {
		t.Fatalf("claude extraArgs=%v — grok flags would be rejected by claude", c.ExtraArgs)
	}
}

// An explicit agent (a stamped session or /agent) wins over the model's agent,
// and a model belonging to the other CLI is dropped rather than passed to a CLI
// that has never heard of it. This is what keeps editing `model` from switching
// the driver on existing threads and destroying their transcripts.
func TestExplicitAgentWinsAndForeignModelIsDropped(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "agent": "grok", "model": "sonnet"`)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ResolveAgentCLI("grok")
	if got.Agent != grokrun.AgentGrok {
		t.Fatalf("agent=%q, a stamped session must keep its CLI", got.Agent)
	}
	if got.Model != "" {
		t.Fatalf("model=%q, a Claude model must not be handed to grok", got.Model)
	}
	// And the reverse.
	cfg, err = loadAgentConfig(t, `, "model": "grok-4"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveAgentCLI("claude"); got.Model != "" {
		t.Fatalf("model=%q, a grok model must not be handed to claude", got.Model)
	}
}

// An unrecognized name is never guessed at: it goes to the fallback agent and is
// passed through so that CLI reports the bad model itself.
func TestUnknownModelUsesFallbackAgentAndPassesThrough(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "agent": "claude", "model": "some-self-hosted-42"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, known := cfg.ModelAgent(); known {
		t.Fatal("an unknown name must not report a known agent")
	}
	got := cfg.ResolveAgentCLI("")
	if got.Agent != grokrun.AgentClaude {
		t.Fatalf("agent=%q, want the configured fallback", got.Agent)
	}
	if got.Model != "some-self-hosted-42" {
		t.Fatalf("model=%q, custom ids must still reach the CLI", got.Model)
	}
}

func TestAgentIncludesAnthropicEnvIsOptInAndClaudeOnly(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentIncludesAnthropicEnv(grokrun.AgentClaude) {
		t.Fatal("ANTHROPIC_* passthrough must be opt-in")
	}

	cfg, err = loadAgentConfig(t, `, "claudeIncludeAnthropicEnv": true`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AgentIncludesAnthropicEnv(grokrun.AgentClaude) {
		t.Fatal("opt-in not honored")
	}
	// grok cannot use those variables, so they stay stripped for it regardless.
	if cfg.AgentIncludesAnthropicEnv(grokrun.AgentGrok) {
		t.Fatal("grok children must never receive ANTHROPIC_*")
	}
}

func TestSnapshotCarriesAgentSettings(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "claudeBin": "/opt/claude", "model": "opus", "summarizeModel": "grok-code-fast-1"`)
	if err != nil {
		t.Fatal(err)
	}
	snap := cfg.Snapshot()
	if snap.ClaudeBin != "/opt/claude" || snap.Model != "opus" {
		t.Fatalf("snapshot=%+v", snap)
	}
	// The derived agents are rendered on the config page so inference is visible.
	if snap.ModelAgent != "claude" || !snap.ModelAgentKnown {
		t.Fatalf("model agent=%q known=%v", snap.ModelAgent, snap.ModelAgentKnown)
	}
	if snap.SummarizeAgent != "grok" || !snap.SummarizeAgentKnown {
		t.Fatalf("summarize agent=%q known=%v", snap.SummarizeAgent, snap.SummarizeAgentKnown)
	}
}

// Live config edits round-trip through save(): the web config page writes the
// file while the bot is running.
func TestAgentSettingsSurviveSaveRoundTrip(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "agent": "claude", "model": "opus", "claudeExtraArgs": ["--effort", "high"], "claudeIncludeAnthropicEnv": true`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultAgent() != grokrun.AgentClaude {
		t.Fatalf("agent=%q", reloaded.Agent)
	}
	cli := reloaded.ResolveAgentCLI("claude")
	if cli.Model != "opus" || len(cli.ExtraArgs) != 2 {
		t.Fatalf("cli=%+v", cli)
	}
	if !reloaded.AgentIncludesAnthropicEnv(grokrun.AgentClaude) {
		t.Fatal("env opt-in lost on save")
	}
}

// The title model is an override, not a replacement: unset means "use the task
// model", which is the behavior every existing config relies on.
func TestResolveSummarizeCLIFallsBackToTaskModel(t *testing.T) {
	cfg, err := loadAgentConfig(t, `, "model": "grok-4"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveSummarizeCLI(""); got.Model != "grok-4" {
		t.Fatalf("summarize model=%q want the task model", got.Model)
	}
}

// A title is not tied to the thread's session (its id is discarded), so the
// title model may name the other CLI and the title call follows it there.
func TestResolveSummarizeCLICanCrossAgents(t *testing.T) {
	cfg, err := loadAgentConfig(t, `,
		"model": "sonnet",
		"summarizeModel": "grok-code-fast-1",
		"claudeBin": "/opt/claude",
		"grokBin": "/usr/local/bin/grok"`)
	if err != nil {
		t.Fatal(err)
	}
	task := cfg.ResolveAgentCLI("")
	if task.Agent != grokrun.AgentClaude || task.Model != "sonnet" {
		t.Fatalf("task cli=%+v", task)
	}
	title := cfg.ResolveSummarizeCLI("")
	if title.Agent != grokrun.AgentGrok || title.Model != "grok-code-fast-1" {
		t.Fatalf("title cli=%+v", title)
	}
	if title.Bin != "/usr/local/bin/grok" {
		t.Fatalf("title must use that agent's binary, got %q", title.Bin)
	}
}

// An empty override must not be confused with "clear the model" when the agent
// default is also empty.
func TestResolveSummarizeCLIAllEmpty(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveSummarizeCLI(""); got.Model != "" {
		t.Fatalf("model=%q want empty so the CLI picks", got.Model)
	}
}

func TestSetAgentSettingsRoundTripsModels(t *testing.T) {
	cfg, err := loadAgentConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.SetAgentSettings(AgentSettings{
		Agent:          "grok",
		Model:          "opus",
		SummarizeModel: "haiku",
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ResolveAgentCLI("").Model; got != "opus" {
		t.Fatalf("task model=%q", got)
	}
	if got := reloaded.ResolveSummarizeCLI(""); got.Model != "haiku" || got.Agent != grokrun.AgentClaude {
		t.Fatalf("summarize cli=%+v", got)
	}
	// Blank claude binary resolves rather than persisting empty.
	if got := reloaded.ResolveAgentCLI("claude").Bin; got != "claude" {
		t.Fatalf("claudeBin=%q", got)
	}
}
