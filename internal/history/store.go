package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Turn is one user→assistant exchange in a Discord thread.
type Turn struct {
	At        string `json:"at"`
	User      string `json:"user,omitempty"`
	UserID    string `json:"userId,omitempty"`
	Prompt    string `json:"prompt"`
	Response  string `json:"response,omitempty"`
	Status    string `json:"status"` // done | cancelled | error
	ExitCode  int    `json:"exitCode,omitempty"`
	// Error is a short human-readable failure reason (max turns, timeout, exit code, …).
	// Empty when Status is done. Older history files may omit this field.
	Error     string `json:"error,omitempty"`
	Elapsed   string `json:"elapsed,omitempty"`
	Project   string `json:"project,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
}

// DisplayError returns a user-visible error line for the history UI.
// Prefers the stored Error field; falls back to exit code for older records.
func (t Turn) DisplayError() string {
	if s := strings.TrimSpace(t.Error); s != "" {
		return s
	}
	switch t.Status {
	case "error":
		if t.ExitCode != 0 {
			return fmt.Sprintf("Grok exited with code %d", t.ExitCode)
		}
		return "Run failed"
	case "cancelled":
		return "Cancelled"
	default:
		return ""
	}
}

// Thread is the full turn log for one Discord thread.
type Thread struct {
	ThreadID string `json:"threadId"`
	Project  string `json:"project,omitempty"`
	Turns    []Turn `json:"turns"`
}

// Summary is a list-row view of a thread log.
type Summary struct {
	ThreadID   string
	Project    string
	LastUser   string
	UpdatedAt  string
	TurnCount  int
	LastPrompt string
	LastStatus string
}

type Store struct {
	mu  sync.Mutex
	dir string
}

func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "history")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Append records a completed turn for a thread.
func (s *Store) Append(threadID string, turn Turn) error {
	if !validThreadID(threadID) {
		return fmt.Errorf("invalid thread id")
	}
	if turn.At == "" {
		turn.At = time.Now().UTC().Format(time.RFC3339)
	}
	if turn.Status == "" {
		turn.Status = "done"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	th, err := s.loadLocked(threadID)
	if err != nil {
		return err
	}
	if th.ThreadID == "" {
		th.ThreadID = threadID
	}
	if turn.Project != "" {
		th.Project = turn.Project
	}
	th.Turns = append(th.Turns, turn)
	return s.saveLocked(th)
}

// Get returns the full turn log for a thread.
func (s *Store) Get(threadID string) (Thread, error) {
	if !validThreadID(threadID) {
		return Thread{}, fmt.Errorf("invalid thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(threadID)
}

// List returns thread summaries newest-first.
func (s *Store) List() ([]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !validThreadID(id) {
			continue
		}
		th, err := s.loadLocked(id)
		if err != nil || len(th.Turns) == 0 {
			continue
		}
		last := th.Turns[len(th.Turns)-1]
		out = append(out, Summary{
			ThreadID:   th.ThreadID,
			Project:    firstNonEmpty(th.Project, last.Project),
			LastUser:   last.User,
			UpdatedAt:  last.At,
			TurnCount:  len(th.Turns),
			LastPrompt: truncate(last.Prompt, 120),
			LastStatus: last.Status,
		})
	}
	slices.SortFunc(out, func(a, b Summary) int {
		switch {
		case a.UpdatedAt == b.UpdatedAt:
			if a.ThreadID < b.ThreadID {
				return -1
			}
			if a.ThreadID > b.ThreadID {
				return 1
			}
			return 0
		case a.UpdatedAt == "":
			return 1
		case b.UpdatedAt == "":
			return -1
		case a.UpdatedAt > b.UpdatedAt:
			return -1
		default:
			return 1
		}
	})
	return out, nil
}

// Delete removes a thread's history file (e.g. optional cleanup).
func (s *Store) Delete(threadID string) error {
	if !validThreadID(threadID) {
		return fmt.Errorf("invalid thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(threadID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) path(threadID string) string {
	return filepath.Join(s.dir, threadID+".json")
}

func (s *Store) loadLocked(threadID string) (Thread, error) {
	raw, err := os.ReadFile(s.path(threadID))
	if err != nil {
		if os.IsNotExist(err) {
			return Thread{ThreadID: threadID, Turns: nil}, nil
		}
		return Thread{}, err
	}
	var th Thread
	if err := json.Unmarshal(raw, &th); err != nil {
		return Thread{}, err
	}
	if th.ThreadID == "" {
		th.ThreadID = threadID
	}
	return th, nil
}

func (s *Store) saveLocked(th Thread) error {
	raw, err := json.MarshalIndent(th, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(s.path(th.ThreadID), raw, 0o600)
}

func validThreadID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}
