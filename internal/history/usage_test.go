package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Usage has to survive the round-trip through disk: it is the only record of what
// a run cost, and the run is gone by the time anyone asks.
func TestUsageRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &Usage{
		InputTokens:         17,
		CacheReadTokens:     8103,
		CacheCreationTokens: 8399,
		OutputTokens:        307,
		TotalTokens:         16826,
		ContextTokens:       8445,
		ContextWindowTokens: 1_000_000,
	}
	if err := s.Append("111", Turn{
		Prompt: "fix bug", Status: "done", Project: "app",
		Agent: "claude", Model: "claude-opus-5", Usage: want,
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh store: reads the file rather than any in-process copy.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	th, err := s2.Get("111")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 1 {
		t.Fatalf("turns=%d", len(th.Turns))
	}
	got := th.Turns[0]
	if got.Agent != "claude" || got.Model != "claude-opus-5" {
		t.Fatalf("agent/model lost: %q/%q", got.Agent, got.Model)
	}
	if got.Usage == nil {
		t.Fatal("usage lost")
	}
	if *got.Usage != *want {
		t.Fatalf("usage=%+v want %+v", *got.Usage, *want)
	}
	if got.Usage.BilledTokens() != 17+8103+8399+307 {
		t.Fatalf("billed=%d", got.Usage.BilledTokens())
	}

	// A turn with no usage must stay absent from the JSON rather than serialize a
	// zeroed object: an older record and "this run cost nothing" are different
	// claims, and every reader distinguishes them by the nil.
	if err := s2.Append("111", Turn{Prompt: "no usage", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "history", "111.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Turns []map[string]json.RawMessage `json:"turns"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Turns) != 2 {
		t.Fatalf("raw turns=%d", len(probe.Turns))
	}
	if _, ok := probe.Turns[1]["usage"]; ok {
		t.Fatalf("nil usage must be omitted: %s", raw)
	}
}

// BilledTokens falls back to the reported total so a CLI that gives no breakdown
// does not look free.
func TestBilledTokensFallsBackToTotal(t *testing.T) {
	if got := (&Usage{TotalTokens: 350}).BilledTokens(); got != 350 {
		t.Fatalf("total-only=%d want 350", got)
	}
	if got := (&Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 9999}).BilledTokens(); got != 105 {
		t.Fatalf("breakdown wins: %d want 105", got)
	}
	var nilUsage *Usage
	if !nilUsage.IsZero() || nilUsage.BilledTokens() != 0 {
		t.Fatal("nil usage must be zero and safe")
	}
	// Occupancy alone still counts as "we know something about this run".
	if (&Usage{ContextTokens: 10}).IsZero() {
		t.Fatal("context-only usage is not zero")
	}
}

func TestWalkVisitsEveryThreadOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a1", "b2", "c3"} {
		if err := s.Append(id, Turn{Prompt: "p", Status: "done", Project: "app"}); err != nil {
			t.Fatal(err)
		}
	}
	// An empty log must not be handed to fn: it carries no turn to account for.
	if err := os.WriteFile(filepath.Join(dir, "history", "empty.json"), []byte(`{"threadId":"empty","turns":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-JSON and traversal-shaped names are skipped like everywhere else.
	if err := os.WriteFile(filepath.Join(dir, "history", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	if err := s.Walk(func(th Thread) error {
		seen[th.ThreadID]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen["a1"] != 1 || seen["b2"] != 1 || seen["c3"] != 1 {
		t.Fatalf("seen=%v", seen)
	}
	// A callback error stops the walk and surfaces.
	stop := errStop{}
	if err := s.Walk(func(Thread) error { return stop }); err != stop {
		t.Fatalf("walk err=%v", err)
	}
	// Missing directory is not an error — nothing has run yet.
	s3, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s3.dir); err != nil {
		t.Fatal(err)
	}
	if err := s3.Walk(func(Thread) error { return stop }); err != nil {
		t.Fatalf("empty walk: %v", err)
	}
}

type errStop struct{}

func (errStop) Error() string { return "stop" }
