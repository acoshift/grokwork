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

	// Thread ownership: first @Grok author; /claim and /hand-off update these.
	// Cancel/reset require owner, co-owner, or Discord moderator override.
	OwnerID    string   `json:"ownerId,omitempty"`
	OwnerName  string   `json:"ownerName,omitempty"`
	CoOwnerIDs []string `json:"coOwnerIds,omitempty"`

	// PRs tracks one or more GitHub pull requests for this thread (multi-repo / multi-PR).
	// Preferred source of truth; legacy single-PR fields below are kept in sync for older data.
	PRs []TrackedPR `json:"prs,omitempty"`

	// Legacy single-PR fields (mirrored from PrimaryPR for backward compatibility).
	PRURL         string `json:"prUrl,omitempty"`
	PRNumber      int    `json:"prNumber,omitempty"`
	PRState       string `json:"prState,omitempty"` // OPEN, MERGED, CLOSED (draft via PRIsDraft)
	PRTitle       string `json:"prTitle,omitempty"`
	PRChecks      string `json:"prChecks,omitempty"`
	PRReview      string `json:"prReview,omitempty"`
	PRHeadSHA     string `json:"prHeadSha,omitempty"`
	PRIsDraft     bool   `json:"prIsDraft,omitempty"`
	PRStatusMsgID string `json:"prStatusMsgId,omitempty"`

	// Legacy CI triage fields (mirrored from primary PR).
	CINotifiedSHA  string `json:"ciNotifiedSha,omitempty"`
	CIAutoFixCount int    `json:"ciAutoFixCount,omitempty"`
	CIAutoFixSHA   string `json:"ciAutoFixSha,omitempty"`
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

// Patch loads the entry, applies fn, and saves. Returns false if missing.
// UpdatedAt is always refreshed when the entry exists.
func (s *Store) Patch(threadID string, fn func(*Entry)) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[threadID]
	if !ok {
		return Entry{}, false, nil
	}
	fn(&e)
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.entries[threadID] = e
	if err := s.save(); err != nil {
		return Entry{}, true, err
	}
	return e, true, nil
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
