package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
)

// sseStream is a small reusable writer for one-shot Grok run streams
// (config suggest, future one-shot tools). Event names:
//
//	status   — {"message":"..."}
//	activity — {"line":"..."}
//	thought  — {"delta":"..."}
//	text     — {"delta":"..."}
//	result   — {"ok":true,"text":"...","message":"...","count":N,...}
//	error    — {"message":"..."}
//	done     — {}
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
	closed  bool
}

func newSSEStream(w http.ResponseWriter) (*sseStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// Prevent intermediary buffering of the first bytes.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseStream{w: w, flusher: flusher}, nil
}

func (s *sseStream) Event(name string, payload any) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("web sse stream marshal: %v", err)
		return false
	}
	if name != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", name); err != nil {
			s.closed = true
			return false
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		s.closed = true
		return false
	}
	s.flusher.Flush()
	return true
}

func (s *sseStream) Status(message string) bool {
	return s.Event("status", map[string]string{"message": message})
}

func (s *sseStream) Activity(line string) bool {
	return s.Event("activity", map[string]string{"line": line})
}

func (s *sseStream) TextDelta(delta string) bool {
	return s.Event("text", map[string]string{"delta": delta})
}

func (s *sseStream) ThoughtDelta(delta string) bool {
	return s.Event("thought", map[string]string{"delta": delta})
}

func (s *sseStream) Result(payload map[string]any) bool {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["ok"]; !ok {
		payload["ok"] = true
	}
	return s.Event("result", payload)
}

func (s *sseStream) Error(message string) bool {
	return s.Event("error", map[string]string{"message": message})
}

func (s *sseStream) Done() bool {
	ok := s.Event("done", map[string]any{})
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return ok
}
