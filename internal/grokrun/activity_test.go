package grokrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseToolCallActivity(t *testing.T) {
	line := `{"timestamp":1,"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"list_dir","rawInput":{"target_directory":"."},"_meta":{"x.ai/tool":{"name":"list_dir","kind":"list"}}}}}`
	got := parseToolCallActivity([]byte(line))
	if !strings.Contains(got, "list_dir") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got, ".") {
		t.Fatalf("expected path detail: %q", got)
	}

	// tool_call_update is ignored (noise).
	upd := `{"params":{"update":{"sessionUpdate":"tool_call_update","title":"list_dir"}}}`
	if got := parseToolCallActivity([]byte(upd)); got != "" {
		t.Fatalf("update should be empty, got %q", got)
	}

	bash := `{"params":{"update":{"sessionUpdate":"tool_call","title":"run_terminal_command","rawInput":{"command":"go test ./...","description":"run tests"},"_meta":{"x.ai/tool":{"name":"run_terminal_command"}}}}}`
	got = parseToolCallActivity([]byte(bash))
	if !strings.Contains(got, "run_terminal_command") || !strings.Contains(got, "go test") {
		t.Fatalf("bash activity: %q", got)
	}
}

func TestWatchSessionTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROK_HOME", home)
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "sess-watch-1"
	dir := filepath.Join(home, "sessions", encodeSessionDir(cwd), sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "updates.jsonl")

	// Pre-existing noise (resume offset should skip).
	if err := os.WriteFile(path, []byte(`{"params":{"update":{"sessionUpdate":"tool_call","title":"old","rawInput":{"command":"old"}}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSessionTools(ctx, cwd, sid, func(line string) {
			got = append(got, line)
		})
	}()

	// Give watcher time to open and note EOF offset.
	time.Sleep(500 * time.Millisecond)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"params":{"update":{"sessionUpdate":"tool_call","title":"read_file","rawInput":{"target_file":"main.go"},"_meta":{"x.ai/tool":{"name":"read_file"}}}}}` + "\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(got) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "read_file") || !strings.Contains(got[0], "main.go") {
		t.Fatalf("line=%q", got[0])
	}
}

func TestNewSessionID(t *testing.T) {
	id := newSessionID()
	// UUID shape: 8-4-4-4-12 hex
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("id=%q", id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("bad lengths id=%q", id)
	}
}
