package grokrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func claudeArgs(opt Options) []string {
	return claudeDriver{}.args(argInput{opt: opt, format: "stream-json"})
}

// argValue returns the value following flag, or "" when absent.
func argValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestClaudeArgsBaseline(t *testing.T) {
	args := claudeArgs(Options{Cwd: "/repo", MaxTurns: 40})

	for _, want := range []string{"--print", "--verbose", "--include-partial-messages"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	if got := argValue(args, "--output-format"); got != "stream-json" {
		t.Errorf("output-format=%q", got)
	}
	if got := argValue(args, "--max-turns"); got != "40" {
		t.Errorf("max-turns=%q", got)
	}
	// grok-only flags must never reach claude: it has no --cwd (cmd.Dir carries the
	// working directory) and no --prompt-file (the prompt goes on stdin).
	for _, never := range []string{"--cwd", "--prompt-file", "--verbatim", "--yolo", "--no-auto-update"} {
		if slices.Contains(args, never) {
			t.Errorf("unexpected grok flag %s in %v", never, args)
		}
	}
	if !(claudeDriver{}).promptOnStdin() {
		t.Error("claude must take the prompt on stdin")
	}
}

func TestClaudeArgsNonStreamOmitsStreamOnlyFlags(t *testing.T) {
	args := claudeDriver{}.args(argInput{opt: Options{MaxTurns: 1}, format: "json"})
	for _, never := range []string{"--verbose", "--include-partial-messages"} {
		if slices.Contains(args, never) {
			t.Errorf("%s only valid with stream-json: %v", never, args)
		}
	}
}

func TestClaudeArgsYoloMapsToBypassPermissions(t *testing.T) {
	args := claudeArgs(Options{Yolo: true})
	if got := argValue(args, "--permission-mode"); got != "bypassPermissions" {
		t.Fatalf("permission-mode=%q args=%v", got, args)
	}
	if slices.Contains(claudeArgs(Options{}), "--permission-mode") {
		t.Fatal("non-yolo runs must not set a permission mode")
	}
}

func TestClaudeArgsToolsOffDisablesMCP(t *testing.T) {
	empty := ""
	args := claudeArgs(Options{Tools: &empty})
	if got := argValue(args, "--tools"); got != "" {
		t.Fatalf("tools=%q, want empty (disables all built-ins)", got)
	}
	// Built-in tools off is not enough: MCP servers still attach their tools, so
	// a tools-restricted run without this flag keeps a write path open.
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("tools-off must disable MCP servers: %v", args)
	}
}

func TestClaudeArgsToolsAllowlistAlsoDisablesMCP(t *testing.T) {
	tools := "Read,Grep,Glob"
	args := claudeArgs(Options{Tools: &tools})
	if got := argValue(args, "--tools"); got != tools {
		t.Fatalf("tools=%q", got)
	}
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("investigate allowlist must disable MCP servers: %v", args)
	}
}

func TestClaudeArgsUnrestrictedToolsOmitsFlags(t *testing.T) {
	args := claudeArgs(Options{})
	for _, never := range []string{"--tools", "--strict-mcp-config"} {
		if slices.Contains(args, never) {
			t.Errorf("nil Tools must stay unrestricted, got %s in %v", never, args)
		}
	}
}

func TestClaudeArgsSessionSelection(t *testing.T) {
	// New session with a caller-supplied id.
	args := claudeDriver{}.args(argInput{format: "stream-json", runSessionID: "sess-new"})
	if got := argValue(args, "--session-id"); got != "sess-new" {
		t.Fatalf("session-id=%q args=%v", got, args)
	}

	// Resume.
	args = claudeArgs(Options{SessionID: "sess-old"})
	if got := argValue(args, "--resume"); got != "sess-old" {
		t.Fatalf("resume=%q args=%v", got, args)
	}
	if slices.Contains(args, "--fork-session") {
		t.Fatal("plain resume must not fork")
	}

	// ForceNewSession means "create a session under this id" (grok's -s). The id
	// is minted before the run and does not exist yet, so resuming it would fail
	// with "No conversation found with session ID" on every new thread.
	args = claudeArgs(Options{SessionID: "sess-fresh", ForceNewSession: true})
	if got := argValue(args, "--session-id"); got != "sess-fresh" {
		t.Fatalf("force-new should create the id, args=%v", args)
	}
	if slices.Contains(args, "--resume") {
		t.Fatalf("force-new must not resume a nonexistent session: %v", args)
	}
}

func TestClaudeArgsCapabilityFlags(t *testing.T) {
	args := claudeArgs(Options{NoSubagents: true, DisableWebSearch: true, NoMemory: true, NoPlan: true})
	deny := argValue(args, "--disallowedTools")
	for _, want := range []string{"Task", "WebSearch", "WebFetch"} {
		if !strings.Contains(deny, want) {
			t.Errorf("disallowedTools=%q missing %s", deny, want)
		}
	}
	if !slices.Contains(args, "--safe-mode") {
		t.Errorf("NoMemory should drop CLAUDE.md/skills/plugins: %v", args)
	}
	// NoPlan has no claude analogue and must not invent a flag.
	if slices.Contains(args, "--no-plan") {
		t.Errorf("unexpected --no-plan: %v", args)
	}
}

func TestClaudeArgsJSONSchemaAndExtraArgs(t *testing.T) {
	args := claudeArgs(Options{JSONSchema: `{"type":"object"}`, ExtraArgs: []string{"--effort", "high"}})
	if got := argValue(args, "--json-schema"); got != `{"type":"object"}` {
		t.Fatalf("json-schema=%q", got)
	}
	if got := argValue(args, "--effort"); got != "high" {
		t.Fatalf("extra args dropped: %v", args)
	}
}

func TestClaudeChildEnvDisablesAutoUpdaterOnce(t *testing.T) {
	env := claudeDriver{}.childEnv([]string{"PATH=/bin"})
	if !slices.Contains(env, "DISABLE_AUTOUPDATER=1") {
		t.Fatalf("env=%v", env)
	}
	again := claudeDriver{}.childEnv(env)
	if n := len(again) - len(env); n != 0 {
		t.Fatalf("re-applied env grew by %d: %v", n, again)
	}
}

// claudeStream is a trimmed capture of a real `claude -p --output-format
// stream-json --include-partial-messages --verbose` run.
const claudeStream = `{"type":"system","subtype":"init","session_id":"11111111-2222-4333-8444-555555555555","tools":["Read"],"model":"claude-opus-5[1m]"}
{"type":"system","subtype":"status","status":"requesting","session_id":"11111111-2222-4333-8444-555555555555"}
{"type":"stream_event","event":{"type":"message_start","message":{"role":"assistant","content":[]}},"session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing options"}},"session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"h"}},"session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"11111111-2222-4333-8444-555555555555"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ello"}},"session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"git status","description":"check tree"}}]},"parent_tool_use_id":null,"session_id":"11111111-2222-4333-8444-555555555555"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"parent_tool_use_id":null,"session_id":"11111111-2222-4333-8444-555555555555"}
{"type":"stream_event","event":{"type":"message_stop"},"session_id":"11111111-2222-4333-8444-555555555555","parent_tool_use_id":null}
{"is_error":false,"num_turns":1,"session_id":"11111111-2222-4333-8444-555555555555","total_cost_usd":0.111906,"usage":{"input_tokens":2,"cache_creation_input_tokens":11121,"cache_read_input_tokens":0,"output_tokens":4},"modelUsage":{"claude-haiku-4-5-20251001":{"contextWindow":200000},"claude-opus-5[1m]":{"contextWindow":1000000}},"subtype":"success","result":"hello","type":"result"}`

func decodeClaude(t *testing.T, raw string, cb streamCallbacks) streamOut {
	t.Helper()
	out, err := decodeStream(strings.NewReader(raw), claudeDriver{}, cb)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestClaudeDecodeStream(t *testing.T) {
	var texts, thoughts, acts []string
	out := decodeClaude(t, claudeStream, streamCallbacks{
		onText:     func(s string) { texts = append(texts, s) },
		onThought:  func(s string) { thoughts = append(thoughts, s) },
		onActivity: func(s string) { acts = append(acts, s) },
	})

	// The reply text arrives three times over the wire — as deltas, as the
	// aggregated assistant message, and again in the result event. Only the
	// deltas may count, or the user sees the answer repeated.
	if out.Text != "hello" {
		t.Fatalf("text=%q want %q", out.Text, "hello")
	}
	if len(texts) != 2 || texts[0] != "h" || texts[1] != "ello" {
		t.Fatalf("deltas=%v", texts)
	}
	if len(thoughts) != 1 || thoughts[0] != "weighing options" {
		t.Fatalf("thoughts=%v", thoughts)
	}
	// system/status=requesting surfaces as activity so the live UI is not
	// blank while waiting on the model; tool_use still follows.
	if len(acts) != 2 || acts[0] != "waiting on model" ||
		!strings.Contains(acts[1], "Bash") || !strings.Contains(acts[1], "git status") {
		t.Fatalf("activity=%v", acts)
	}
	if out.SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("session=%q", out.SessionID)
	}
	if out.NumTurns != 1 {
		t.Fatalf("numTurns=%d", out.NumTurns)
	}
	if out.MaxTurnsReached {
		t.Fatal("unexpected MaxTurnsReached")
	}
}

func TestClaudeDecodeUsageCountsCacheCreation(t *testing.T) {
	out := decodeClaude(t, claudeStream, streamCallbacks{})
	if out.Usage == nil {
		t.Fatal("no usage")
	}
	// A cold cache puts nearly the whole prompt in cache_creation_input_tokens
	// (2 input vs 11121 created here); ignoring it reports a ~2-token prompt.
	if got := out.Usage.PromptTokens(); got != 11123 {
		t.Fatalf("promptTokens=%d want 11123", got)
	}
	if out.ContextTokensUsed != 11127 {
		t.Fatalf("contextUsed=%d want 11127", out.ContextTokensUsed)
	}
	// Side calls bill a small model with a narrower window; the session's real
	// window is the widest one billed.
	if out.ContextWindowTokens != 1_000_000 {
		t.Fatalf("contextWindow=%d want 1000000", out.ContextWindowTokens)
	}
	res := Result{ContextTokensUsed: out.ContextTokensUsed, ContextWindowTokens: out.ContextWindowTokens}
	if got := res.ContextSummary(); got != "11k/1M" {
		t.Fatalf("summary=%q", got)
	}
}

func TestClaudeDecodeMaxTurns(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial work"}},"parent_tool_use_id":null}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":40,"session_id":"s-max","result":"max turns"}`,
	}, "\n")
	out := decodeClaude(t, raw, streamCallbacks{})
	if !out.MaxTurnsReached {
		t.Fatal("expected MaxTurnsReached")
	}
	want := "partial work\n\n" + MaxTurnsUserMessage
	if out.Text != want {
		t.Fatalf("text=%q want %q", out.Text, want)
	}
	if out.NumTurns != 40 || out.SessionID != "s-max" {
		t.Fatalf("turns=%d session=%q", out.NumTurns, out.SessionID)
	}
}

// Every error subtype leaves "result" null and puts the reason in "errors" — the
// shape below is what the CLI actually emitted when forced to fail. Reading only
// "result" made a failed run reach Discord as "(empty response)" with no reason,
// which is the one thing the operator needs.
func TestClaudeDecodeErrorResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{{
		name: "errors array",
		raw:  `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":null,"errors":["credit balance too low"],"terminal_reason":"turn_setup_failed"}`,
		want: "credit balance too low",
	}, {
		name: "several errors joined",
		raw:  `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["tool crashed","retry exhausted"]}`,
		want: "tool crashed; retry exhausted",
	}, {
		name: "budget exhausted",
		raw:  `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"session_id":"s1","result":null,"errors":["Reached maximum budget ($0.01)"],"terminal_reason":"budget_exhausted"}`,
		want: "Reached maximum budget ($0.01)",
	}, {
		// No reason anywhere: still say something rather than nothing.
		name: "terminal reason only",
		raw:  `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","terminal_reason":"tool_deferred_unavailable"}`,
		want: "run ended: tool_deferred_unavailable",
	}, {
		// is_error with subtype=success is the one shape that does carry result.
		name: "error with result",
		raw:  `{"type":"result","subtype":"success","is_error":true,"session_id":"s1","result":"api error: overloaded"}`,
		want: "api error: overloaded",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := decodeClaude(t, c.raw, streamCallbacks{})
			if out.Text != c.want {
				t.Fatalf("text=%q want %q", out.Text, c.want)
			}
			if out.MaxTurnsReached {
				t.Fatal("plain error is not a turn limit")
			}
		})
	}
}

// The result event's usage is a cumulative bill across every API call, and a cached
// prefix is re-counted each turn, so summing it overstates context roughly in
// proportion to turn count. Only the last iteration is resident context.
func TestClaudeDecodeContextUsesFinalIteration(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"s1","result":"ok",` +
		`"usage":{"input_tokens":17,"cache_creation_input_tokens":8399,"cache_read_input_tokens":8103,"output_tokens":307,` +
		`"iterations":[{"input_tokens":9,"cache_creation_input_tokens":8103,"cache_read_input_tokens":0,"output_tokens":269},` +
		`{"input_tokens":8,"cache_creation_input_tokens":296,"cache_read_input_tokens":8103,"output_tokens":38}]},` +
		`"modelUsage":{"claude-haiku-4-5-20251001":{"contextWindow":200000}}}`
	out := decodeClaude(t, raw, streamCallbacks{})
	// Final iteration: 8 + 296 + 8103 + 38.
	if out.ContextTokensUsed != 8445 {
		t.Fatalf("contextUsed=%d want 8445 (the cumulative sum 16826 would be ~2x reality)", out.ContextTokensUsed)
	}
	// Billing totals are still the cumulative figures — those are what cost money.
	if out.Usage == nil || out.Usage.OutputTokens != 307 {
		t.Fatalf("usage must stay cumulative: %+v", out.Usage)
	}
}

// A run is several assistant messages: tool preamble, then the answer. Gluing them
// produced output like "I'll read the file.`a.txt` contains one line" — two
// sentences fused mid-line. Every run with tool use hits this.
func TestClaudeDecodeSeparatesAssistantMessages(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"I'll read the file."}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"message_stop"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"It contains one line."}},"parent_tool_use_id":null}`,
		`{"type":"result","subtype":"success","result":"ignored","session_id":"s1"}`,
	}, "\n")
	out := decodeClaude(t, raw, streamCallbacks{})
	if want := "I'll read the file.\n\nIt contains one line."; out.Text != want {
		t.Fatalf("text=%q want %q", out.Text, want)
	}
	// A leading message_start must not open the reply with blank lines.
	if strings.HasPrefix(out.Text, "\n") {
		t.Fatalf("reply starts with a separator: %q", out.Text)
	}
}

// Activity lines are rendered into Discord, where a local path must never appear
// (CLAUDE.md). claude's file tools take absolute paths, and the detail is truncated
// to its first 60 characters — so the raw form both leaked the private worktree
// path and spent the whole budget on a prefix common to every file, dropping the
// filename.
func TestClaudeActivityHidesLocalPaths(t *testing.T) {
	cwd := "/Users/dev/Projects/grokwork/data/worktrees/app/1234567890"
	raw := `{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"Read",` +
		`"input":{"file_path":"` + cwd + `/internal/web/live.go"}}]}}`
	var lines []string
	decodeClaude(t, raw, streamCallbacks{
		onActivity: func(s string) { lines = append(lines, s) },
		cwd:        cwd,
	})
	if len(lines) != 1 {
		t.Fatalf("activity=%v", lines)
	}
	if want := "Read: internal/web/live.go"; lines[0] != want {
		t.Fatalf("activity=%q want %q", lines[0], want)
	}
	for _, bad := range []string{"/Users/", "worktrees", cwd} {
		if strings.Contains(lines[0], bad) {
			t.Fatalf("activity leaks %q: %q", bad, lines[0])
		}
	}
}

// A path outside cwd is left alone rather than mangled — but it is still a local
// path, so this documents that the guard is cwd-relative only.
func TestClaudeActivityLeavesForeignPathsAlone(t *testing.T) {
	var lines []string
	raw := `{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`
	decodeClaude(t, raw, streamCallbacks{
		onActivity: func(s string) { lines = append(lines, s) },
		cwd:        "/Users/dev/app",
	})
	if len(lines) != 1 || lines[0] != "Read: /etc/hosts" {
		t.Fatalf("activity=%v", lines)
	}
}

func TestClaudeDecodeSkipsSubagentOutput(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"main"}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"SUBAGENT"}},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"SUBAGENT REPLY"}]},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"result","subtype":"success","result":"main","session_id":"s1"}`,
	}, "\n")
	out := decodeClaude(t, raw, streamCallbacks{})
	if out.Text != "main" {
		t.Fatalf("subagent output leaked into the reply: %q", out.Text)
	}
}

// Without --include-partial-messages there are no deltas, so the aggregated
// assistant message is the only copy of the text and must be taken.
func TestClaudeDecodeWithoutPartialMessages(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"full reply"}]},"parent_tool_use_id":null,"session_id":"s1"}`,
		`{"type":"result","subtype":"success","result":"full reply","session_id":"s1"}`,
	}, "\n")
	out := decodeClaude(t, raw, streamCallbacks{})
	if out.Text != "full reply" {
		t.Fatalf("text=%q", out.Text)
	}
}

func TestClaudeDecodeFinalJSON(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"session_id":"s-json","result":"done","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":1,"output_tokens":7}}`)
	out, ok := claudeDriver{}.decodeFinal(raw)
	if !ok {
		t.Fatal("decodeFinal rejected valid output")
	}
	if out.Text != "done" || out.SessionID != "s-json" || out.NumTurns != 3 {
		t.Fatalf("out=%+v", out)
	}
	if out.Usage == nil || out.Usage.PromptTokens() != 16 {
		t.Fatalf("usage=%+v", out.Usage)
	}
	if _, ok := (claudeDriver{}).decodeFinal([]byte("not json")); ok {
		t.Fatal("decodeFinal accepted non-JSON")
	}
}

// TestClaudeRunEndToEnd drives Run against a stub binary to cover the plumbing
// the decode tests cannot: the prompt reaching the child on stdin, and the
// result assembling into a Result.
func TestClaudeRunEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	// Echoes back what it read on stdin so the test can assert prompt delivery.
	script := `#!/bin/sh
prompt=$(cat)
printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"got:%s"}},"parent_tool_use_id":null}\n' "$prompt"
printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"stub-session","result":"ignored"}\n'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var deltas []string
	res := Run(context.Background(), Options{
		Agent:       AgentClaude,
		Bin:         bin,
		Prompt:      "fix #42 at https://ex.com/a?x=1&b=2",
		Cwd:         dir,
		MaxTurns:    1,
		Timeout:     30 * time.Second,
		OnTextDelta: func(d string) { deltas = append(deltas, d) },
	})

	if res.Code != 0 {
		t.Fatalf("code=%d text=%q stderr=%q", res.Code, res.Text, res.Stderr)
	}
	// Shell/CLI parsing must not mangle #, ? or & on the way to the child.
	if want := "got:fix #42 at https://ex.com/a?x=1&b=2"; res.Text != want {
		t.Fatalf("text=%q want %q", res.Text, want)
	}
	if len(deltas) != 1 {
		t.Fatalf("deltas=%v", deltas)
	}
	if res.SessionID != "stub-session" {
		t.Fatalf("session=%q", res.SessionID)
	}
}

// Crash recovery re-drives a run with the session id it minted before the dead
// attempt started, so the session usually already exists — and claude refuses to
// create a taken id. Without the retry, every claude recovery fails instantly and
// the thread loses its context. The stub reproduces the real CLI: exit 1 with
// "Session ID … is already in use." on stderr, then success once resumed.
func TestClaudeRunRetriesWhenSessionAlreadyExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	argvLog := filepath.Join(dir, "argv.log")
	script := `#!/bin/sh
cat >/dev/null
echo "$@" >> ` + argvLog + `
case "$*" in
  *--session-id*)
    echo "Error: Session ID abc is already in use." >&2
    exit 1 ;;
  *--resume*)
    printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"abc","result":"resumed ok"}\n'
    exit 0 ;;
esac
exit 3
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Options{
		Agent:           AgentClaude,
		Bin:             bin,
		Prompt:          "continue",
		Cwd:             dir,
		SessionID:       "abc",
		ForceNewSession: true,
		Timeout:         30 * time.Second,
		OnTextDelta:     func(string) {},
	})

	if res.Code != 0 {
		t.Fatalf("code=%d text=%q stderr=%q", res.Code, res.Text, res.Stderr)
	}
	if res.Text != "resumed ok" {
		t.Fatalf("text=%q, want the resumed reply", res.Text)
	}
	if res.SessionID != "abc" {
		t.Fatalf("session=%q, want the same id retained", res.SessionID)
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 2 {
		t.Fatalf("child ran %d times, want exactly 2 (create then resume):\n%s", len(lines), logged)
	}
	if !strings.Contains(lines[0], "--session-id abc") {
		t.Fatalf("first attempt argv=%q, want --session-id", lines[0])
	}
	if !strings.Contains(lines[1], "--resume abc") {
		t.Fatalf("retry argv=%q, want --resume", lines[1])
	}
}

// The retry is narrow: any other failure must surface as-is, or a genuinely broken
// run would be executed twice and reported as whatever the second attempt did.
func TestClaudeRunDoesNotRetryOtherFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	argvLog := filepath.Join(dir, "argv.log")
	script := `#!/bin/sh
cat >/dev/null
echo "$@" >> ` + argvLog + `
echo "Error: something else went wrong" >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Options{
		Agent:           AgentClaude,
		Bin:             bin,
		Prompt:          "continue",
		Cwd:             dir,
		SessionID:       "abc",
		ForceNewSession: true,
		Timeout:         30 * time.Second,
		OnTextDelta:     func(string) {},
	})
	if res.Code == 0 {
		t.Fatalf("want the failure to surface, got code=0 text=%q", res.Text)
	}
	logged, _ := os.ReadFile(argvLog)
	if n := len(strings.Split(strings.TrimSpace(string(logged)), "\n")); n != 1 {
		t.Fatalf("child ran %d times, want 1 (no retry)", n)
	}
}

func TestClaudeDecodeMalformedLineIsReported(t *testing.T) {
	raw := "not-json\n" + `{"type":"result","subtype":"success","result":"ok","session_id":"s1"}`
	out, err := decodeStream(strings.NewReader(raw), claudeDriver{}, streamCallbacks{})
	if err == nil {
		t.Fatal("expected a parse note for the malformed line")
	}
	if out.Text != "ok" {
		t.Fatalf("text=%q — a bad line must not abort decoding", out.Text)
	}
}

func TestClaudeStatusActivity(t *testing.T) {
	if got := claudeStatusActivity("requesting"); got != "waiting on model" {
		t.Fatalf("requesting: %q", got)
	}
	if got := claudeStatusActivity("  "); got != "" {
		t.Fatalf("blank: %q", got)
	}
	if got := claudeStatusActivity("thinking"); got != "thinking" {
		t.Fatalf("passthrough: %q", got)
	}
}

func TestClaudeDecodeSystemStatusAsActivity(t *testing.T) {
	raw := `{"type":"system","subtype":"status","status":"requesting","session_id":"s1"}
{"type":"result","subtype":"success","session_id":"s1","result":"hi","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}
`
	var acts []string
	out := decodeClaude(t, raw, streamCallbacks{
		onActivity: func(line string) { acts = append(acts, line) },
	})
	if out.SessionID != "s1" {
		t.Fatalf("session=%q", out.SessionID)
	}
	if len(acts) != 1 || acts[0] != "waiting on model" {
		t.Fatalf("activity=%v", acts)
	}
}
