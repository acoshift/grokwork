package web

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEStreamEvents(t *testing.T) {
	w := httptest.NewRecorder()
	s, err := newSSEStream(w)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Status("starting") {
		t.Fatal("status")
	}
	if !s.TextDelta("hello") {
		t.Fatal("text")
	}
	if !s.Result(map[string]any{"text": "unit | go test", "count": 1}) {
		t.Fatal("result")
	}
	if !s.Done() {
		t.Fatal("done")
	}
	// After Done, further writes fail.
	if s.Status("again") {
		t.Fatal("want closed after done")
	}

	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}
	events := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events[strings.TrimPrefix(line, "event: ")]++
		}
	}
	for _, name := range []string{"status", "text", "result", "done"} {
		if events[name] != 1 {
			t.Fatalf("event %q count=%d body=%q", name, events[name], body)
		}
	}
	if !strings.Contains(body, `"delta":"hello"`) {
		t.Fatalf("missing delta: %q", body)
	}
}
