package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// SchemaVersion is stamped on every run record so a future format change can be
// detected rather than misread.
const SchemaVersion = 1

// Status is a run's or a step's lifecycle state.
//
// The set mirrors internal/runjournal's: a deploy has the same "was it still
// going when we died" question, and a reader of one should recognise the other.
type Status string

const (
	StatusPending     Status = "pending"
	StatusRunning     Status = "running"
	StatusCancelling  Status = "cancelling"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
	StatusSkipped     Status = "skipped"
)

// Terminal reports whether a status is final.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted, StatusBlocked:
		return true
	}
	return false
}

// ErrSkipUpdate aborts an Update without writing, mirroring runjournal.
var ErrSkipUpdate = errors.New("deploy: skip update")

// ErrRunNotFound is returned for an unknown run id.
var ErrRunNotFound = errors.New("deploy: run not found")

// StepRecord is one step's frozen definition and outcome.
type StepRecord struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs"`
	Status    Status `json:"status"`
	ExitCode  int    `json:"exitCode,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	LogBytes  int64  `json:"logBytes,omitempty"`
	LogTrunc  bool   `json:"logTrunc,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Run is one deploy of one service to one environment at one commit.
//
// Steps are frozen at trigger time from the manifest at SHA, so a redeploy
// replays exactly what ran even if the branch has moved on.
type Run struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Project       string `json:"project"`
	Repo          string `json:"repo"`
	Service       string `json:"service"`
	Env           string `json:"env"`
	Ref           string `json:"ref"`
	SHA           string `json:"sha"`
	ShortSHA      string `json:"shortSha"`
	Subject       string `json:"subject,omitempty"`
	// ServiceDir is the service working directory relative to the checkout,
	// frozen with the steps so a redeploy uses the same one.
	ServiceDir string `json:"serviceDir,omitempty"`

	Status      Status       `json:"status"`
	Steps       []StepRecord `json:"steps"`
	CurrentStep int          `json:"currentStep"`

	ActorID   string `json:"actorId,omitempty"`
	ActorName string `json:"actorName,omitempty"`

	QueuedAt  string `json:"queuedAt"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`

	// CheckoutPath is the ephemeral detached worktree; removed on terminal.
	CheckoutPath string `json:"checkoutPath,omitempty"`
	// PID is the live step's process-group leader, for the restart sweep.
	PID int `json:"pid,omitempty"`
	// Host guards against re-driving another machine's work.
	Host string `json:"host,omitempty"`

	// RefCheck records how the ref gate was satisfied ("" or "waived_redeploy").
	RefCheck string `json:"refCheck,omitempty"`
	// RedeployOf names the run this one replays.
	RedeployOf string `json:"redeployOf,omitempty"`
	// SupersededBy names a later successful run on the same lane.
	SupersededBy string `json:"supersededBy,omitempty"`

	Error string `json:"error,omitempty"`
}

// serviceDir returns the frozen working directory, defaulting to the root.
func (r Run) serviceDir() string {
	if r.ServiceDir == "" {
		return "."
	}
	return r.ServiceDir
}

// Lane returns the concurrency key: one active run per service+environment.
func (r Run) Lane() string { return LaneKey(r.Project, r.Repo, r.Service, r.Env) }

// LaneKey builds the concurrency key.
func LaneKey(project, repo, service, env string) string {
	if repo == "" {
		repo = "."
	}
	return strings.Join([]string{project, repo, service, env}, "/")
}

// Elapsed reports how long the run took, or has been running.
func (r Run) Elapsed() time.Duration {
	start := parseTime(r.StartedAt)
	if start.IsZero() {
		return 0
	}
	end := parseTime(r.EndedAt)
	if end.IsZero() {
		end = time.Now().UTC()
	}
	return end.Sub(start)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

var runIDRe = regexp.MustCompile(`^d_[0-9a-f]{32}$`)

// NewRunID mints a run id. The d_ prefix keeps deploy artifacts obviously
// distinct from the w_ web units and Discord snowflakes elsewhere in data/.
func NewRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable and must not yield a colliding id.
		panic("deploy: crypto/rand: " + err.Error())
	}
	return "d_" + hex.EncodeToString(b)
}

// Store persists deploy runs, one atomic file each.
//
// Modelled on internal/runjournal, the only store in this repo with atomic
// tmp+rename writes, a status enum, an Update RMW API, a schema version and a
// path-sanitizing id guard. There is no separate queue file: the active and
// queued runs of a lane are those whose status is running or pending, so the
// records cannot disagree with an index.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore opens (creating if needed) dataDir/deploys.
func NewStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "deploys", "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is the runs directory (tests and the checkout sweeper use it).
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) (string, error) {
	if !runIDRe.MatchString(id) {
		return "", fmt.Errorf("deploy: invalid run id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Create writes a new run record.
func (s *Store) Create(r Run) error {
	if _, err := s.path(r.ID); err != nil {
		return err
	}
	r.SchemaVersion = SchemaVersion
	if r.QueuedAt == "" {
		r.QueuedAt = nowStamp()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(r)
}

func (s *Store) saveLocked(r Run) error {
	p, err := s.path(r.ID)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// Atomic: a crash mid-write must not leave a half-parsed run record that
	// the restart sweep would misread.
	return os.Rename(tmp, p)
}

// Get loads one run.
func (s *Store) Get(id string) (Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *Store) loadLocked(id string) (Run, bool, error) {
	p, err := s.path(id)
	if err != nil {
		return Run{}, false, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Run{}, false, nil
		}
		return Run{}, false, err
	}
	var r Run
	if err := json.Unmarshal(raw, &r); err != nil {
		return Run{}, false, fmt.Errorf("deploy: run %s: %w", id, err)
	}
	return r, true, nil
}

// Update applies fn to a run and persists, under one lock.
//
// fn returning ErrSkipUpdate aborts without writing, which is how a caller
// declines a transition it has decided is a no-op.
func (s *Store) Update(id string, fn func(*Run) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunNotFound
	}
	if err := fn(&r); err != nil {
		if errors.Is(err, ErrSkipUpdate) {
			return nil
		}
		return err
	}
	return s.saveLocked(r)
}

// List returns every run, newest queued first.
func (s *Store) List() ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Run
	for _, e := range entries {
		// Per-run log directories live alongside the records.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		r, ok, err := s.loadLocked(id)
		if err != nil || !ok {
			// A torn or foreign file must not break the whole list.
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt > out[j].QueuedAt })
	return out, nil
}

// ListForLane returns a lane's runs, newest first, capped by limit (0 = all).
func (s *Store) ListForLane(lane string, limit int) ([]Run, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := slices.DeleteFunc(all, func(r Run) bool { return r.Lane() != lane })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes a run record and its logs.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(filepath.Join(s.dir, id))
}

// Dots are excluded so a step name containing ".." cannot reach a filename.
var logSlugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// StepLogPath is where one step's output lives.
func (s *Store) StepLogPath(id string, idx int, stepName string) (string, error) {
	if !runIDRe.MatchString(id) {
		return "", fmt.Errorf("deploy: invalid run id %q", id)
	}
	slug := logSlugRe.ReplaceAllString(strings.ToLower(stepName), "-")
	if slug == "" {
		slug = "step"
	}
	return filepath.Join(s.dir, id, "steps", fmt.Sprintf("%02d-%s.log", idx, slug)), nil
}

// CreateStepLog opens a step's log for appending, creating its directory.
func (s *Store) CreateStepLog(id string, idx int, stepName string) (*os.File, error) {
	p, err := s.StepLogPath(id, idx, stepName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// ReadStepLogTail returns the last maxBytes of a step's log, plus whether it
// was clipped.
//
// The live fragment re-renders whole rather than appending deltas: htmx bakes a
// live region's hx-get URL at render time, so an "after=N" baked into it would
// be replayed unchanged on every SSE tick — the same bytes forever. Re-reading a
// bounded tail each tick is what the session page does too, and it is correct by
// construction.
func (s *Store) ReadStepLogTail(id string, idx int, stepName string, maxBytes int64) ([]byte, bool, error) {
	p, err := s.StepLogPath(id, idx, stepName)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := st.Size()
	start := int64(0)
	clipped := false
	if maxBytes > 0 && size > maxBytes {
		start = size - maxBytes
		clipped = true
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, false, err
	}
	buf := make([]byte, size-start)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, clipped, nil
	}
	return buf[:n], clipped, nil
}

// LiveLogTailBytes bounds what the live fragment re-sends each tick.
const LiveLogTailBytes = 64 << 10

// ReadStepLog returns bytes after the given offset plus the new offset.
//
// Offset-based rather than whole-file so the page can tail a running step, and
// so a viewer arriving mid-run passes 0 and gets everything.
func (s *Store) ReadStepLog(id string, idx int, stepName string, after int64) ([]byte, int64, error) {
	p, err := s.StepLogPath(id, idx, stepName)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			// A step that has not started yet has no log; that is not an error.
			return nil, after, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if after < 0 || after > size {
		// The file shrank (a rewritten run) — restart from the beginning rather
		// than returning garbage.
		after = 0
	}
	if after == size {
		return nil, size, nil
	}
	if _, err := f.Seek(after, 0); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, size-after)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, after, err
	}
	return buf[:n], after + int64(n), nil
}
