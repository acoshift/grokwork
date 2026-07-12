package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	SessionID string `json:"sessionId"`
	Project   string `json:"project"`
	Cwd       string `json:"cwd"`
	LastUser  string `json:"lastUser,omitempty"`
	UpdatedAt string `json:"updatedAt"`
}

// Store maps Discord thread ID → Grok session metadata.
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
