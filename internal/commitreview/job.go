// Package commitreview runs AI commit reviews (read-only tools for context) and files GitHub issues per finding.
package commitreview

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

// Job statuses.
const (
	StatusQueued          = "queued"
	StatusRunning         = "running"
	StatusCreatingIssues  = "creating_issues"
	StatusDone            = "done"
	StatusFailed          = "failed"
)

// Finding is one review finding before/after issue create.
type Finding struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Severity    string   `json:"severity"`
	Paths       []string `json:"paths,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	IssueNumber int      `json:"issueNumber,omitempty"`
	IssueURL    string   `json:"issueURL,omitempty"`
	CreateError string   `json:"createError,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
}

// Job is a persisted commit review run.
type Job struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Actor     string    `json:"actor"`
	Project   string    `json:"project"`
	Owner     string    `json:"owner"`
	Repo      string    `json:"repo"`
	SHA       string    `json:"sha"`
	ShortSHA  string    `json:"shortSha"`
	Subject   string    `json:"subject,omitempty"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
}

// Store persists jobs as JSON under dataDir/commit-reviews/.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore roots jobs under dataDir/commit-reviews.
func NewStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "commit-reviews")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("commitreview store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save writes job JSON (0600).
func (s *Store) Save(j *Job) error {
	if s == nil || j == nil {
		return fmt.Errorf("nil store or job")
	}
	j.UpdatedAt = time.Now().UTC()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = j.UpdatedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(j.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(j.ID))
}

// Get loads a job by id.
func (s *Store) Get(id string) (*Job, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, "..") {
		return nil, fmt.Errorf("invalid job id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// LatestForSHA returns the most recently updated job for project/owner/repo/sha, or nil.
func (s *Store) LatestForSHA(project, owner, repo, sha string) (*Job, error) {
	if s == nil {
		return nil, nil
	}
	project = strings.TrimSpace(project)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	sha = strings.TrimSpace(sha)
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var best *Job
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(b, &j); err != nil {
			continue
		}
		if j.Project != project || j.Owner != owner || j.Repo != repo {
			continue
		}
		if j.SHA != sha && j.ShortSHA != sha && !strings.HasPrefix(j.SHA, sha) {
			continue
		}
		if best == nil || j.UpdatedAt.After(best.UpdatedAt) {
			cp := j
			best = &cp
		}
	}
	return best, nil
}

// ActiveForSHA reports whether a non-terminal job exists for this SHA.
func (s *Store) ActiveForSHA(project, owner, repo, sha string) (*Job, error) {
	j, err := s.LatestForSHA(project, owner, repo, sha)
	if err != nil || j == nil {
		return j, err
	}
	switch j.Status {
	case StatusQueued, StatusRunning, StatusCreatingIssues:
		return j, nil
	default:
		return nil, nil
	}
}

// NewJobID returns a random hex id.
func NewJobID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
