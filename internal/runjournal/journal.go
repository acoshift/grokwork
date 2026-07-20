// Package runjournal persists active Grok runs and follow-up queues for crash recovery.
package runjournal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Status is the durable lifecycle of a task or journal active slot.
type Status string

const (
	StatusPending       Status = "pending"
	StatusRunning       Status = "running"
	StatusCancelling    Status = "cancelling"
	StatusInterrupted   Status = "interrupted"
	StatusBlockedOrphan Status = "blocked_orphan"
	StatusDone          Status = "done"
	StatusFailed        Status = "failed"
)

// SchemaVersion is written on every Save.
const SchemaVersion = 1

// Actor is who started a task (serializable).
type Actor struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// TaskRecord is one durable task intent (active or queued).
type TaskRecord struct {
	ID               string   `json:"id"`
	Status           Status   `json:"status"`
	Prompt           string   `json:"prompt"`
	Project          string   `json:"project"`
	ProjectCwd       string   `json:"projectCwd,omitempty"`
	Source           string   `json:"source"`
	Origin           string   `json:"origin,omitempty"`
	Actor            Actor    `json:"actor"`
	CreatedBy        string   `json:"createdBy,omitempty"`
	CreatedByName    string   `json:"createdByName,omitempty"`
	DiscordURL       string   `json:"discordUrl,omitempty"`
	TriggerMsgID     string   `json:"triggerMsgId,omitempty"`
	StatusMsgID      string   `json:"statusMsgId,omitempty"`
	AttachmentPaths  []string `json:"attachmentPaths,omitempty"`
	ReferencedPrompt string   `json:"referencedPrompt,omitempty"`
	CreatedAt        string   `json:"createdAt"`
	StartedAt        string   `json:"startedAt,omitempty"`
	Attempt          int      `json:"attempt"`
}

// Journal is one thread's durable active run + FIFO queue.
type Journal struct {
	ThreadID      string       `json:"threadId"`
	Version       int          `json:"version"`
	Active        *TaskRecord  `json:"active,omitempty"`
	Queue         []TaskRecord `json:"queue,omitempty"`
	SessionID     string       `json:"sessionId,omitempty"`
	WorktreeCwd   string       `json:"worktreeCwd,omitempty"`
	Branch        string       `json:"branch,omitempty"`
	GrokPID       int          `json:"grokPid,omitempty"`
	Host          string       `json:"host,omitempty"`
	HeartbeatAt   string       `json:"heartbeatAt,omitempty"`
	Generation    uint64       `json:"generation"`
	BlockedReason string       `json:"blockedReason,omitempty"`
	UpdatedAt     string       `json:"updatedAt"`
}

// Store persists journals under dataDir/runs/.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New roots journals under dataDir/runs.
func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runjournal: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the runs directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *Store) path(threadID string) string {
	return filepath.Join(s.dir, sanitizeThreadID(threadID)+".json")
}

// TaskFilesDir returns data/runs/<threadId>/files/<taskId>/.
func (s *Store) TaskFilesDir(threadID, taskID string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.dir, sanitizeThreadID(threadID), "files", sanitizeThreadID(taskID))
}

// ThreadDir returns data/runs/<threadId>/.
func (s *Store) ThreadDir(threadID string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.dir, sanitizeThreadID(threadID))
}

func sanitizeThreadID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "..", "_")
	if id == "" {
		return "_empty"
	}
	return id
}

// Load returns a journal by thread id.
func (s *Store) Load(threadID string) (Journal, bool, error) {
	if s == nil {
		return Journal{}, false, fmt.Errorf("nil store")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return Journal{}, false, fmt.Errorf("empty thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(threadID)
}

func (s *Store) loadLocked(threadID string) (Journal, bool, error) {
	b, err := os.ReadFile(s.path(threadID))
	if err != nil {
		if os.IsNotExist(err) {
			return Journal{}, false, nil
		}
		return Journal{}, false, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return Journal{}, false, err
	}
	if j.ThreadID == "" {
		j.ThreadID = threadID
	}
	return j, true, nil
}

// Save writes journal JSON atomically (0600).
func (s *Store) Save(j *Journal) error {
	if s == nil || j == nil {
		return fmt.Errorf("nil store or journal")
	}
	j.ThreadID = strings.TrimSpace(j.ThreadID)
	if j.ThreadID == "" {
		return fmt.Errorf("empty thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(j)
}

func (s *Store) saveLocked(j *Journal) error {
	if j.Version == 0 {
		j.Version = SchemaVersion
	}
	j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	p := s.path(j.ThreadID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Update loads, applies fn, and saves under one store mutex (RMW-safe).
// If the journal is missing, fn receives a zero Journal with ThreadID set; return
// a non-nil error from fn to abort without writing. Returning ErrSkipUpdate skips save.
func (s *Store) Update(threadID string, fn func(*Journal) error) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("empty thread id")
	}
	if fn == nil {
		return fmt.Errorf("nil update fn")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok, err := s.loadLocked(threadID)
	if err != nil {
		return err
	}
	if !ok {
		j = Journal{ThreadID: threadID, Version: SchemaVersion}
	}
	if err := fn(&j); err != nil {
		if err == ErrSkipUpdate {
			return nil
		}
		return err
	}
	j.ThreadID = threadID
	return s.saveLocked(&j)
}

// ErrSkipUpdate aborts Update without writing.
var ErrSkipUpdate = fmt.Errorf("runjournal: skip update")

// Delete removes the journal file and optional thread files tree.
func (s *Store) Delete(threadID string) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("empty thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(threadID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(s.ThreadDir(threadID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List loads all journals in the runs directory.
func (s *Store) List() ([]Journal, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Journal
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var j Journal
		if err := json.Unmarshal(b, &j); err != nil {
			continue
		}
		if j.ThreadID == "" {
			j.ThreadID = strings.TrimSuffix(e.Name(), ".json")
		}
		out = append(out, j)
	}
	return out, nil
}

// HasWork reports whether a journal exists with non-terminal active work, queue, or blocked orphan.
func (s *Store) HasWork(threadID string) bool {
	j, ok, err := s.Load(threadID)
	if err != nil || !ok {
		return false
	}
	return j.HasWork()
}

// HasWork reports non-terminal work on this journal.
func (j Journal) HasWork() bool {
	if j.Active != nil {
		switch j.Active.Status {
		case StatusDone, StatusFailed:
		default:
			return true
		}
	}
	return len(j.Queue) > 0
}

// RemoveTaskFiles deletes files for one task id (best-effort).
func (s *Store) RemoveTaskFiles(threadID, taskID string) {
	if s == nil || taskID == "" {
		return
	}
	_ = os.RemoveAll(s.TaskFilesDir(threadID, taskID))
}

// NewTaskID returns a random hex id for a task record.
func NewTaskID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (i * 4))
		}
	}
	return hex.EncodeToString(b[:])
}
