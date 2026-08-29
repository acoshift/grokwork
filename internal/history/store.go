package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
)

// Turn is one user→assistant exchange in a Discord thread.
type Turn struct {
	At       string `json:"at"`
	User     string `json:"user,omitempty"`
	UserID   string `json:"userId,omitempty"`
	Prompt   string `json:"prompt"`
	Response string `json:"response,omitempty"`
	Status   string `json:"status"` // done | cancelled | error
	ExitCode int    `json:"exitCode,omitempty"`
	// Error is a short human-readable failure reason (max turns, timeout, exit code, …).
	// Empty when Status is done. Older history files may omit this field.
	Error     string `json:"error,omitempty"`
	Elapsed   string `json:"elapsed,omitempty"`
	Project   string `json:"project,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	// Wave 1 classification (optional on older records).
	RunKind string `json:"runKind,omitempty"` // fix|investigate|explain|fix_ci|…
	Mode    string `json:"mode,omitempty"`    // session mode at turn time
	Phase   string `json:"phase,omitempty"`
	Preset  string `json:"preset,omitempty"`
	// Agent / Model name the CLI and model this turn ran on. Recorded per turn
	// rather than read back from the session because rates are per model and the
	// session's stamp is not a historical record: a thread created before models
	// were selectable carries no stamp at all, and what a rollup needs is the
	// name that was billed, not the name configured today.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Usage is the token accounting for this turn's run. Nil on records written
	// before spend tracking, and on runs whose CLI reported nothing.
	Usage *Usage `json:"usage,omitempty"`
	// Attachments are files the user handed this turn (Discord/web). Bytes live
	// under data/history/<threadId>/<n>/; this slice is the allowlist the
	// session page serves from. Older records omit it.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Artifacts are files the agent sent to this session during this turn.
	// Bytes live under data/history/<threadId>/out/; Thread.Artifacts is the
	// session allowlist the download route serves from. This slice is the
	// subset to render on the turn's assistant bubble.
	Artifacts []Attachment `json:"artifacts,omitempty"`
}

// Attachment is one file persisted with a turn or the session.
type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size,omitzero"`
	// Rel is a worktree-relative label for display (never a host-absolute path).
	Rel string `json:"rel,omitempty"`
}

// File is a source to copy into a turn's durable files dir or the session
// artifact store. Bytes, when set, are used instead of Path (MCP content).
type File struct {
	Path        string
	Name        string
	ContentType string
	Rel         string
	Bytes       []byte
}

// Usage is what one turn's run cost in tokens, plus how full the context window
// was when it ended.
//
// The billed classes and ContextTokens answer different questions and neither may
// be derived from the other. The billed numbers are cumulative across every API
// call the run made — a cached prefix is re-sent and re-charged on every turn, so
// an invoice counts it once per call — which is exactly what a spend rollup must
// sum. ContextTokens is occupancy at a single instant (the end of the run), so
// adding the calls up overstates it roughly in proportion to the turn count: a
// real 2-turn claude run measured 16,826 cumulative against 8,445 resident. See
// grokrun's Result.Usage vs Result.ContextTokensUsed, which keep the same split.
type Usage struct {
	InputTokens         int `json:"inputTokens,omitempty"`
	CacheReadTokens     int `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int `json:"cacheCreationTokens,omitempty"`
	OutputTokens        int `json:"outputTokens,omitempty"`
	// TotalTokens is the run total as the CLI reported it, kept verbatim instead
	// of recomputed: a CLI that reports only a total and no breakdown would
	// otherwise record a turn that cost zero tokens.
	TotalTokens int `json:"totalTokens,omitempty"`
	// ContextTokens / ContextWindowTokens describe the end-of-run window, not the
	// bill. Reasoning tokens have no field: no provider prices them as a separate
	// class, and both CLIs already fold them into the totals above, so a fifth
	// class would be double counting.
	ContextTokens       int `json:"contextTokens,omitempty"`
	ContextWindowTokens int `json:"contextWindowTokens,omitempty"`
}

// BilledTokens is every token the run paid for. Falls back to the CLI-reported
// total when no per-class breakdown was recorded.
func (u *Usage) BilledTokens() int {
	if u == nil {
		return 0
	}
	if n := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens; n > 0 {
		return n
	}
	return u.TotalTokens
}

// IsZero reports a usage that carries no numbers — an older record, or a run
// whose CLI said nothing about tokens.
func (u *Usage) IsZero() bool {
	return u == nil || (u.BilledTokens() == 0 && u.ContextTokens == 0)
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
	// Artifacts is the session-level allowlist of agent-sent files. Bytes live
	// under data/history/<threadId>/out/; the allowlist is data/history/<threadId>/artifacts.json.
	// Filled on read (sidecar, or a legacy artifacts field on this JSON). Never
	// written back onto the turn log — a mid-run file must not rewrite turns.
	Artifacts []Attachment `json:"artifacts,omitempty"`
}

// Summary is a list-row view of a thread log.
// Session* fields are filled by the web layer from sessionstore (not persisted here).
type Summary struct {
	ThreadID   string
	Project    string
	LastUser   string
	UpdatedAt  string
	TurnCount  int
	LastPrompt string
	LastStatus string

	// Optional sessionstore overlay for sessions list/detail chrome.
	Goal       string // sticky session goal (list identity; not from history)
	Label      string // effective lifecycle label (open, done, …)
	Mode       string // case | fix | …
	Phase      string // case phase (incl. closed)
	Resolution string // case resolution when closed
	// Primary tracked PR (if any) for list badges/links.
	PRNumber int
	PRState  string // OPEN | MERGED | CLOSED
	PROwner  string
	PRRepo   string
	PRURL    string
	PRTitle  string
	HasPRs   bool
	// AllPRsTerminal is true when the unit tracks ≥1 PR and none are open.
	// Used by the sessions Active filter: shipped units drop off after recency
	// even when the lifecycle label is still needs_review / in_progress.
	AllPRsTerminal bool
	// Running is true when the bot has an active agent job on this thread
	// (web overlay from StatusSnapshot; not stored in history JSON).
	Running bool
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
	return s.AppendFiles(threadID, turn, nil)
}

// AppendFiles records a completed turn and copies files into the thread's
// durable files dir. A copy failure for one file skips that file rather than
// dropping the turn — the transcript is the record; attachments are extras.
func (s *Store) AppendFiles(threadID string, turn Turn, files []File) error {
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
	turnN := len(th.Turns) + 1
	if atts := copyTurnFiles(s.filesDir(threadID, turnN), files); len(atts) > 0 {
		turn.Attachments = atts
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
	th, err := s.loadLocked(threadID)
	if err != nil {
		return Thread{}, err
	}
	s.attachArtifactsLocked(&th)
	return th, nil
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

// Walk calls fn once per thread log.
//
// Threads are loaded one file at a time and dropped again, so an aggregate over
// every turn (a spend rollup) never holds more than one transcript in memory —
// a full log carries every prompt and response, which is megabytes of text to
// read past in order to add up integers.
//
// fn runs outside the store lock so it may call back into the store; the cost is
// that a thread deleted mid-walk is skipped rather than erroring, and one
// appended mid-walk may or may not be seen. Both are fine for a report.
func (s *Store) Walk(fn func(Thread) error) error {
	s.mu.Lock()
	entries, err := os.ReadDir(s.dir)
	s.mu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !validThreadID(id) {
			continue
		}
		s.mu.Lock()
		th, err := s.loadLocked(id)
		s.mu.Unlock()
		if err != nil {
			continue
		}
		if len(th.Turns) == 0 {
			continue
		}
		if err := fn(th); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a thread's history file and its attachment files dir.
func (s *Store) Delete(threadID string) error {
	if !validThreadID(threadID) {
		return fmt.Errorf("invalid thread id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jsonErr := os.Remove(s.path(threadID))
	if os.IsNotExist(jsonErr) {
		jsonErr = nil
	}
	dirErr := os.RemoveAll(s.filesRoot(threadID))
	if os.IsNotExist(dirErr) {
		dirErr = nil
	}
	return errors.Join(jsonErr, dirErr)
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
	// One-release migrate: c85ee47 wrote Artifacts onto this file. Move them
	// to the sidecar before stripping so a later Get still sees them.
	if len(th.Artifacts) > 0 {
		if arts, err := s.loadArtifactsLocked(th.ThreadID); err == nil && len(arts) == 0 {
			if err := s.saveArtifactsLocked(th.ThreadID, th.Artifacts); err != nil {
				return err
			}
		}
	}
	th.Artifacts = nil
	raw, err := json.MarshalIndent(th, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicfile.Write(s.path(th.ThreadID), raw, 0o600)
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
