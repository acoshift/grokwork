package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Entry struct {
	SessionID      string `json:"sessionId"`
	Project        string `json:"project"`
	Cwd            string `json:"cwd"` // worktree path when isolated
	MainCwd        string `json:"mainCwd,omitempty"`
	WorktreeBranch string `json:"worktreeBranch,omitempty"`
	LastUser       string `json:"lastUser,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

type Store struct {
	mu       sync.Mutex
	filePath string
	entries  map[string]Entry
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		filePath: filepath.Join(dataDir, "sessions.json"),
		entries:  map[string]Entry{},
	}
	_ = s.load()
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, &s.entries)
}

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, raw, 0o600)
}

func (s *Store) Get(threadID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[threadID]
	return e, ok
}

func (s *Store) Set(threadID string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.entries[threadID] = e
	return s.save()
}

func (s *Store) Delete(threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, threadID)
	return s.save()
}

// Listed is a session entry with its Discord thread id for history views.
type Listed struct {
	ThreadID string
	Entry
}

// List returns all sessions sorted by UpdatedAt descending (newest first).
func (s *Store) List() []Listed {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Listed, 0, len(s.entries))
	for id, e := range s.entries {
		out = append(out, Listed{ThreadID: id, Entry: e})
	}
	sortListed(out)
	return out
}

// Count returns the number of stored sessions.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func sortListed(out []Listed) {
	slices.SortFunc(out, func(a, b Listed) int {
		// Newest first; empty timestamps last.
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
}
