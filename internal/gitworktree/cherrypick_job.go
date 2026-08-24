package gitworktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
)

const (
	JobStatusConflict = "conflict"
	JobStatusPushed   = "pushed"
	JobStatusAborted  = "aborted"
	JobStatusExpired  = "expired"
	JobStatusFailed   = "failed"

	CherryPickJobTTL = 24 * time.Hour
)

var cherryPickJobIDRe = regexp.MustCompile(`^cp_[0-9a-f]{32}$`)

// Job is a parked cherry-pick waiting on conflict resolution.
type Job struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Owner     string    `json:"owner,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	RepoPath  string    `json:"repoPath"`
	Checkout  string    `json:"checkout"`
	Target    string    `json:"target"`
	FromSHA   string    `json:"fromSHA"`
	Ordered   []string  `json:"ordered,omitempty"`
	Picked    []string  `json:"picked,omitempty"`
	Skipped   []string  `json:"skipped,omitempty"`
	Current   string    `json:"current,omitempty"`
	Remaining []string  `json:"remaining,omitempty"`
	Files     []string  `json:"files,omitempty"`
	Status    string    `json:"status"`
	ActorID   string    `json:"actorId,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

func (j Job) Open() bool {
	return j.Status == JobStatusConflict
}

func (j Job) clone() Job {
	j.Ordered = slices.Clone(j.Ordered)
	j.Picked = slices.Clone(j.Picked)
	j.Skipped = slices.Clone(j.Skipped)
	j.Remaining = slices.Clone(j.Remaining)
	j.Files = slices.Clone(j.Files)
	return j
}

func ValidJobID(id string) bool {
	return cherryPickJobIDRe.MatchString(strings.TrimSpace(id))
}

func jobsDir(dataDir string) string {
	return filepath.Join(strings.TrimSpace(dataDir), "cherrypick", "jobs")
}

func jobFile(dataDir, id string) string {
	return filepath.Join(jobsDir(dataDir), sanitizePathSegment(id)+".json")
}

var jobStoreMu sync.Mutex

func SaveJob(dataDir string, j Job) error {
	j.ID = strings.TrimSpace(j.ID)
	if !ValidJobID(j.ID) {
		return fmt.Errorf("invalid cherry-pick job id")
	}
	j.UpdatedAt = time.Now().UTC()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = j.UpdatedAt
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(jobsDir(dataDir), 0o700); err != nil {
		return err
	}
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()
	return atomicfile.Write(jobFile(dataDir, j.ID), raw, 0o600)
}

func LoadJob(dataDir, id string) (Job, error) {
	id = strings.TrimSpace(id)
	if !ValidJobID(id) {
		return Job{}, fmt.Errorf("invalid cherry-pick job id")
	}
	jobStoreMu.Lock()
	raw, err := os.ReadFile(jobFile(dataDir, id))
	jobStoreMu.Unlock()
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return Job{}, err
	}
	if j.ID != id || j.Project == "" {
		return Job{}, fmt.Errorf("corrupt cherry-pick job")
	}
	return j.clone(), nil
}

func ListJobs(dataDir string) ([]Job, error) {
	dir := jobsDir(dataDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Job
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		j, err := LoadJob(dataDir, id)
		if err != nil {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

// OpenJobForTarget returns the open conflict job for this checkout+target, if any.
func OpenJobForTarget(dataDir, project, repoPath, target string) (Job, bool) {
	list, err := ListJobs(dataDir)
	if err != nil {
		return Job{}, false
	}
	project = strings.TrimSpace(project)
	repoPath = strings.TrimSpace(repoPath)
	target = strings.TrimSpace(target)
	for _, j := range list {
		if j.Open() && j.Project == project && j.Target == target && sameRepoPath(j.RepoPath, repoPath) {
			return j, true
		}
	}
	return Job{}, false
}

func sameRepoPath(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return a == b
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return aa == bb
}

// SweepExpiredCherryPicks aborts conflict jobs older than CherryPickJobTTL.
func SweepExpiredCherryPicks(ctx context.Context, dataDir string) int {
	list, err := ListJobs(dataDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range list {
		if !j.Open() {
			continue
		}
		if !j.UpdatedAt.IsZero() && time.Since(j.UpdatedAt) < CherryPickJobTTL {
			continue
		}
		_ = AbortCherryPick(ctx, j.RepoPath, j.Checkout)
		j.Status = JobStatusExpired
		j.Error = "expired"
		_ = SaveJob(dataDir, j)
		n++
	}
	return n
}
