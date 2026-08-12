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
	ActionConfigAddProject       = "config.add_project"
	ActionConfigRemoveProject    = "config.remove_project"
	ActionConfigSetLinear        = "config.set_project_linear"
	ActionConfigSetClickUp       = "config.set_project_clickup"
	ActionConfigAddUser          = "config.add_user"
	ActionConfigRemoveUser       = "config.remove_user"
	ActionConfigSetTeam          = "config.set_team"
	ActionConfigRemoveTeam       = "config.remove_team"
	ActionConfigAddTeamMember    = "config.add_team_member"
	ActionConfigRemoveTeamMember = "config.remove_team_member"
	ActionConfigAddChannel       = "config.add_channel"
	ActionConfigRemoveChannel    = "config.remove_channel"
	ActionConfigSettings         = "config.settings"
	ActionWorktreePrune          = "worktree.prune"
	ActionWorktreePruneIdle      = "worktree.prune_idle"
	ActionLoginFail              = "login.fail"
	ActionLoginOK                = "login.ok"
	ActionIssueComment           = "issue.comment"
	ActionIssueClose             = "issue.close"
	ActionIssueCreate            = "issue.create"
	ActionIssueChecklistLink     = "issue.checklist.link"  // bot appended a session link to a tasklist line
	ActionIssueChecklistCheck    = "issue.checklist.check" // bot flipped a tasklist box after all session PRs merged
	ActionPRComment              = "pr.comment"
	ActionPRClose                = "pr.close"
	ActionPRMerge                = "pr.merge"
	ActionPRReviewSubmit         = "pr.review.submit"
	ActionPRReviewRequest        = "pr.review.request"
	ActionPRReviewCancel         = "pr.review.cancel"
	ActionPRReviewObsolete       = "pr.review.obsolete"
	ActionPRReviewGitHub         = "pr.review.github" // real gh pr review as the host gh user; unlike ActionPRReviewSubmit it can satisfy branch protection
	ActionSessionStart           = "session.start"    // Fix with Grok / web session start
	ActionSessionCancel          = "session.cancel"
	ActionSessionReset           = "session.reset"
	ActionSessionDequeue         = "session.dequeue"
	ActionSessionLabel           = "session.label"
	ActionSessionGoal            = "session.goal"
	ActionSessionClaim           = "session.claim"
	ActionSessionHandOff         = "session.handoff"
	ActionSessionWatch           = "session.watch"
	ActionSessionUnwatch         = "session.unwatch"
	ActionSessionIssueLink       = "session.issue.link"   // bind a GitHub/Linear ticket to a unit
	ActionSessionIssueUnlink     = "session.issue.unlink" // drop one binding (or all, detail.scope=all)
	ActionCommitReviewStart      = "commit.review.start"
	ActionPRReviewStart          = "pr.review.start" // agentic PR review → PR comment
	ActionGitFetch               = "git.fetch"
	ActionGitSync                = "git.sync"               // fetch + merge origin primary into a unit branch
	ActionGitCheckpoint          = "git.checkpoint"         // bot-owned local checkpoint ref
	ActionGitCheckpointRestore   = "git.checkpoint_restore" // hard reset a unit worktree to a checkpoint
	ActionVerifyRun              = "verify.run"             // project verify commands (shell, no model)
	ActionAccessDeny             = "access.deny"            // refused before any command ran
	// Identity linking (internal/identity). A link changes which grants, threads
	// and spend a login resolves to, so both the grant and every refusal are
	// events — a refused link is exactly what a "someone tried to attach a login
	// to my account" report is checked against.
	ActionIdentityLink   = "identity.link"
	ActionIdentityUnlink = "identity.unlink"
	// Case actions. Both surfaces write these strings; keep them here so the
	// Discord and web halves of one workflow cannot drift into two action names.
	ActionCaseEscalate       = "case.escalate"
	ActionCaseAnswer         = "case.answer"
	ActionCaseClose          = "case.close"
	ActionCaseReopen         = "case.reopen"
	ActionCaseCustomerUpdate = "case.customer_update"
	ActionCaseLink           = "case.link"
	ActionCaseUnlink         = "case.unlink"
	// GitHub Actions (workflow_dispatch from the web Actions page).
	ActionActionsDispatch = "actions.dispatch"
	// Project file storage (GCS). Local staging paths never enter details.
	ActionStorageUpload           = "storage.upload"
	ActionStorageDelete           = "storage.delete"
	ActionConfigSetProjectStorage       = "config.set_project_storage"
	ActionConfigSetGlobalStorage        = "config.set_global_storage"
	ActionConfigSetProjectPrimaryBranch = "config.set_project_primary_branch"
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

	// Scrub here rather than at the call sites. Most of what lands in Error is
	// subprocess stderr — git, gh, a project's verify command — whose wording is
	// nobody's to control, and the audit file is kept, so one forgotten call site
	// is a permanent leak. Doing it in Append also makes the guarantee uniform
	// across surfaces: the Discord path scrubbed while the web path wrote gh
	// stderr verbatim, so the same action logged a path or not depending on which
	// button the operator happened to press.
	ev.Error = ScrubPaths(ev.Error)
	ev.Detail = scrubDetail(ev.Detail)

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
	for line := range strings.SplitSeq(string(raw), "\n") {
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
