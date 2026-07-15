package grokrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsumeStream(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"thought","data":"Thinking"}`,
		`{"type":"text","data":"Hello"}`,
		`{"type":"text","data":" world"}`,
		`{"type":"end","sessionId":"sess-1","stopReason":"EndTurn","num_turns":3,"usage":{"input_tokens":100,"cache_read_input_tokens":200,"output_tokens":50,"total_tokens":350}}`,
		``,
		`not-json`,
	}, "\n")

	var texts, thoughts []string
	out, err := consumeStream(strings.NewReader(raw), func(d string) {
		texts = append(texts, d)
	}, func(d string) {
		thoughts = append(thoughts, d)
	}, nil)
	if out.Text != "Hello world" {
		t.Fatalf("text=%q", out.Text)
	}
	if out.SessionID != "sess-1" {
		t.Fatalf("session=%q", out.SessionID)
	}
	if out.NumTurns != 3 {
		t.Fatalf("numTurns=%d", out.NumTurns)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 350 || out.Usage.PromptTokens() != 300 {
		t.Fatalf("usage=%+v", out.Usage)
	}
	if len(texts) != 2 || texts[0] != "Hello" || texts[1] != " world" {
		t.Fatalf("texts=%v", texts)
	}
	if len(thoughts) != 1 || thoughts[0] != "Thinking" {
		t.Fatalf("thoughts=%v", thoughts)
	}
	if err == nil {
		t.Fatal("expected parse note for malformed line")
	}
}

func TestConsumeStreamErrorEvent(t *testing.T) {
	raw := `{"type":"error","message":"boom"}` + "\n"
	out, err := consumeStream(strings.NewReader(raw), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "boom" {
		t.Fatalf("text=%q", out.Text)
	}
}

func TestConsumeStreamToolActivity(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"tool","name":"bash","data":"git status"}`,
		`{"type":"text","data":"ok"}`,
		`{"type":"end","sessionId":"s1"}`,
	}, "\n")
	var acts []string
	out, err := consumeStream(strings.NewReader(raw), nil, nil, func(s string) {
		acts = append(acts, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "ok" {
		t.Fatalf("text=%q", out.Text)
	}
	if len(acts) != 1 || !strings.Contains(acts[0], "bash") {
		t.Fatalf("acts=%v", acts)
	}
}

func TestFormatTokenCount(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		999:       "999",
		1000:      "1k",
		1500:      "1.5k",
		10000:     "10k",
		148262:    "148k",
		500000:    "500k",
		1_000_000: "1M",
		1_500_000: "1.5M",
	}
	for n, want := range cases {
		if got := formatTokenCount(n); got != want {
			t.Errorf("formatTokenCount(%d)=%q want %q", n, got, want)
		}
	}
}

func TestContextSummary(t *testing.T) {
	r := Result{ContextTokensUsed: 4787, ContextWindowTokens: 500000}
	if got := r.ContextSummary(); got != "4.8k/500k" {
		t.Fatalf("got %q", got)
	}
	r = Result{Usage: &Usage{InputTokens: 100, CacheReadInputTokens: 200, TotalTokens: 350}}
	if got := r.ContextSummary(); got != "~300" {
		t.Fatalf("fallback got %q", got)
	}
	if got := (Result{}).ContextSummary(); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestEncodeSessionDir(t *testing.T) {
	got := encodeSessionDir("/Users/acoshift/Projects/acoshift/grok-discord")
	want := "%2FUsers%2Facoshift%2FProjects%2Facoshift%2Fgrok-discord"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEnrichContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROK_HOME", home)
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "sess-test-1"
	sigDir := filepath.Join(home, "sessions", encodeSessionDir(cwd), sid)
	if err := os.MkdirAll(sigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sig := `{"contextTokensUsed":148262,"contextWindowTokens":500000}`
	if err := os.WriteFile(filepath.Join(sigDir, "signals.json"), []byte(sig), 0o644); err != nil {
		t.Fatal(err)
	}

	res := Result{SessionID: sid}
	enrichContext(&res, cwd)
	if res.ContextTokensUsed != 148262 || res.ContextWindowTokens != 500000 {
		t.Fatalf("got used=%d window=%d", res.ContextTokensUsed, res.ContextWindowTokens)
	}
	if sum := res.ContextSummary(); sum != "148k/500k" {
		t.Fatalf("summary=%q", sum)
	}
}

func TestWritePromptFilePreservesSpecialChars(t *testing.T) {
	prompt := "fix #42 and https://ex.com/a?x=1&b=2 with \"quotes\" and\nnewlines"
	path, cleanup, err := writePromptFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != prompt {
		t.Fatalf("got %q want %q", string(raw), prompt)
	}
	// NUL stripped
	path2, cleanup2, err := writePromptFile("a\x00b#c")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != "ab#c" {
		t.Fatalf("nul strip got %q", string(raw2))
	}
}
