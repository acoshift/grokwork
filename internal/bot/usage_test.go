package bot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// One real claude run, three assistant messages, three API calls. The per-call
// figures below are the shape the CLI actually emits: every call after the first
// re-reads the whole cached prefix, so cache_read grows monotonically and the
// top-level totals are the *sum* of all three calls.
//
// Resident context at the end is the last call alone: 6 + 8399 + 120 + 40.
// The cumulative bill is 23 + 16502 + 8519 + 347 — three times larger, and the
// number a naive implementation would show as "context used".
const (
	claudeResidentContext   = 6 + 8399 + 120 + 40     // 8565
	claudeCumulativeBilled  = 23 + 16502 + 8519 + 347 // 25391
	claudeCumulativePrompt  = 23 + 16502 + 8519       // what PromptTokens() returns
	claudeStubContextWindow = 1_000_000
)

// claudeStub writes a fake claude CLI that replays the run above.
func claudeStub(t *testing.T) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a shell script")
	}
	dir = t.TempDir()
	bin = filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
cat >/dev/null
cat <<'EOF'
{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Reading the file."}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Patching it."}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Done."}},"parent_tool_use_id":null}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"session_id":"stub-session","result":"Done.","usage":{"input_tokens":23,"cache_creation_input_tokens":8519,"cache_read_input_tokens":16502,"output_tokens":347,"iterations":[{"input_tokens":9,"cache_creation_input_tokens":8103,"cache_read_input_tokens":0,"output_tokens":269},{"input_tokens":8,"cache_creation_input_tokens":296,"cache_read_input_tokens":8103,"output_tokens":38},{"input_tokens":6,"cache_creation_input_tokens":120,"cache_read_input_tokens":8399,"output_tokens":40}]},"modelUsage":{"claude-opus-5":{"contextWindow":1000000}}}
EOF
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func runClaudeStub(t *testing.T) grokrun.Result {
	t.Helper()
	bin, dir := claudeStub(t)
	// OnTextDelta is what selects the streaming path, which is the path real runs
	// take (Discord and the web session page both stream).
	res := grokrun.Run(context.Background(), grokrun.Options{
		Agent:       grokrun.AgentClaude,
		Bin:         bin,
		Prompt:      "fix the flaky test",
		Cwd:         dir,
		MaxTurns:    3,
		Timeout:     30 * time.Second,
		OnTextDelta: func(string) {},
	})
	if res.Code != 0 {
		t.Fatalf("stub run failed: code=%d stderr=%q", res.Code, res.Stderr)
	}
	return res
}

// The trap: claude's result usage is a cumulative bill across every API call, so
// using it as context occupancy overstates the window by roughly the turn count.
// Three assistant messages must not report three times the resident tokens.
func TestTurnUsageContextIsNotTheCumulativeBill(t *testing.T) {
	res := runClaudeStub(t)

	// Sanity: the run really did produce the cumulative/resident split, so the
	// assertions below are testing the projection and not a stub that lost data.
	if res.Usage == nil || res.Usage.PromptTokens() != claudeCumulativePrompt {
		t.Fatalf("stub usage=%+v want prompt %d", res.Usage, claudeCumulativePrompt)
	}
	if res.ContextTokensUsed != claudeResidentContext {
		t.Fatalf("driver context=%d want %d", res.ContextTokensUsed, claudeResidentContext)
	}

	u := turnUsage(res)
	if u == nil {
		t.Fatal("usage must be recorded")
	}
	if u.ContextTokens != claudeResidentContext {
		t.Fatalf("contextTokens=%d want %d (the cumulative %d would be ~3x reality)",
			u.ContextTokens, claudeResidentContext, claudeCumulativePrompt)
	}
	if u.ContextTokens >= claudeCumulativePrompt {
		t.Fatalf("contextTokens=%d is at or above the cumulative prompt %d — occupancy was taken from the bill",
			u.ContextTokens, claudeCumulativePrompt)
	}
	if u.ContextWindowTokens != claudeStubContextWindow {
		t.Fatalf("contextWindow=%d", u.ContextWindowTokens)
	}

	// The bill, by contrast, IS the cumulative figure: every API call was charged,
	// including each re-read of the cached prefix. Recording the resident figure
	// here would under-report the cost of a long run by the same factor.
	if u.BilledTokens() != claudeCumulativeBilled {
		t.Fatalf("billed=%d want %d", u.BilledTokens(), claudeCumulativeBilled)
	}
	if u.InputTokens != 23 || u.CacheCreationTokens != 8519 || u.CacheReadTokens != 16502 || u.OutputTokens != 347 {
		t.Fatalf("billed classes=%+v", *u)
	}
	if u.BilledTokens() <= u.ContextTokens {
		t.Fatal("a 3-call run must bill more than it leaves resident")
	}
}

// A run that reported no tokens records no usage: an older turn and a run that
// cost nothing must stay distinguishable, and a zeroed object would collapse them.
func TestTurnUsageNilWhenNothingReported(t *testing.T) {
	if u := turnUsage(grokrun.Result{}); u != nil {
		t.Fatalf("empty result=%+v", u)
	}
	// grok reports a total with no per-class breakdown; that still counts.
	u := turnUsage(grokrun.Result{Usage: &grokrun.Usage{TotalTokens: 350}})
	if u == nil || u.BilledTokens() != 350 {
		t.Fatalf("total-only=%+v", u)
	}
	// Occupancy with no bill is still worth recording (grok's signals.json path).
	u = turnUsage(grokrun.Result{ContextTokensUsed: 4787, ContextWindowTokens: 500_000})
	if u == nil || u.ContextTokens != 4787 || u.BilledTokens() != 0 {
		t.Fatalf("context-only=%+v", u)
	}
}

// The history record is what a rollup reads months later, so the whole chain —
// driver decode, projection, persistence — has to carry the split intact.
func TestRecordTurnPersistsUsageAndModel(t *testing.T) {
	res := runClaudeStub(t)

	dir := t.TempDir()
	proj := filepath.Join(dir, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": proj}),
		DataDir:  dir,
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A stamped session: the agent and model a rollup prices come from the same
	// stamp the run resolved through, never from whatever config says today.
	if err := store.Set("t1", sessionstore.Entry{
		SessionID: "s1", Project: "app", Agent: "claude", Model: "claude-opus-5",
	}); err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, store, hist)
	b.recordTurnActorPolicy("t1", Actor{ID: "u1", DisplayName: "alice"}, nil,
		"app", "fix the flaky test", res, 42*time.Second, RunPolicy{}, nil)

	th, err := hist.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 1 {
		t.Fatalf("turns=%d", len(th.Turns))
	}
	turn := th.Turns[0]
	if turn.Agent != "claude" || turn.Model != "claude-opus-5" {
		t.Fatalf("agent/model=%q/%q", turn.Agent, turn.Model)
	}
	if turn.Usage == nil {
		t.Fatal("usage not persisted")
	}
	if turn.Usage.BilledTokens() != claudeCumulativeBilled {
		t.Fatalf("billed=%d want %d", turn.Usage.BilledTokens(), claudeCumulativeBilled)
	}
	if turn.Usage.ContextTokens != claudeResidentContext {
		t.Fatalf("contextTokens=%d want %d", turn.Usage.ContextTokens, claudeResidentContext)
	}
}
