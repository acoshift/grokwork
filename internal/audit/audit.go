// Package audit appends structured JSONL events under data/audit/.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Common action kinds (extensible — any non-empty Action is allowed).
const (
	ActionConfigAddProject    = "config.add_project"
	ActionConfigRemoveProject = "config.remove_project"
	ActionConfigSetLinear     = "config.set_project_linear"
	ActionConfigAddUser       = "config.add_user"
	ActionConfigRemoveUser    = "config.remove_user"
	ActionConfigAddRole       = "config.add_role"
	ActionConfigRemoveRole    = "config.remove_role"
	ActionConfigAddChannel    = "config.add_channel"
	ActionConfigRemoveChannel = "config.remove_channel"
	ActionConfigSettings      = "config.settings"
	ActionWorktreePrune       = "worktree.prune"
	ActionWorktreePruneIdle   = "worktree.prune_idle"
	ActionLoginFail           = "login.fail"
	ActionLoginOK             = "login.ok"
	ActionIssueComment        = "issue.comment"
	ActionIssueClose          = "issue.close"
	ActionIssueCreate         = "issue.create"
	ActionPRComment           = "pr.comment"
	ActionPRClose             = "pr.close"
	ActionPRMerge             = "pr.merge"
	ActionSessionStart        = "session.start" // Fix with Grok / web session start
	ActionCommitReviewStart   = "commit.review.start"
)

// ActorAnonymous is used when web auth is off or no session is present.
const ActorAnonymous = "anonymous"

// Event is one append-only audit record.
type Event struct {
	Time   time.Time      `json:"time"`
	Action string         `json:"action"`
	Actor  string         `json:"actor"` // Discord snowflake, display name, or ActorAnonymous
	Role   string         `json:"role,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
}

// Logger writes date-partitioned JSONL under dataDir/audit/YYYY-MM-DD.jsonl (0600).
type Logger struct {
	dir string
	mu  sync.Mutex
	now func() time.Time // tests inject
}

// New returns a logger rooted at dataDir/audit. dataDir is typically config.DataDir.
func New(dataDir string) (*Logger, error) {
	dir := filepath.Join(dataDir, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit mkdir: %w", err)
	}
	return &Logger{dir: dir, now: time.Now}, nil
}

// Dir returns the audit directory path.
func (l *Logger) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Append writes one event. Nil logger is a no-op.
func (l *Logger) Append(ev Event) error {
	if l == nil {
		return nil
	}
	ev.Action = strings.TrimSpace(ev.Action)
	if ev.Action == "" {
		return fmt.Errorf("audit: empty action")
	}
	if strings.TrimSpace(ev.Actor) == "" {
		ev.Actor = ActorAnonymous
	}
	if ev.Time.IsZero() {
		if l.now != nil {
			ev.Time = l.now().UTC()
		} else {
			ev.Time = time.Now().UTC()
		}
	} else {
		ev.Time = ev.Time.UTC()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	path := filepath.Join(l.dir, ev.Time.Format("2006-01-02")+".jsonl")
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit open: %w", err)
	}
	defer f.Close()
	// Ensure mode stays 0600 even if umask widened create.
	_ = f.Chmod(0o600)
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

// ReadDay loads all events from a single day file (for tests / future UI).
func (l *Logger) ReadDay(day time.Time) ([]Event, error) {
	if l == nil {
		return nil, fmt.Errorf("audit: nil logger")
	}
	path := filepath.Join(l.dir, day.UTC().Format("2006-01-02")+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return out, fmt.Errorf("audit parse: %w", err)
		}
		out = append(out, ev)
	}
	return out, nil
}
