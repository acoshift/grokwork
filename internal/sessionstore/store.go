package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoshift/grokwork/internal/atomicfile"
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
	// Cancel/reset require owner, co-owner, or a project admin (a team whose
	// capability template grants adminProject).
	OwnerID    string   `json:"ownerId,omitempty"`
	OwnerName  string   `json:"ownerName,omitempty"`
	CoOwnerIDs []string `json:"coOwnerIds,omitempty"`

	// Dual-surface workflow metadata (web + Discord). Preserved across session Set rebuilds.
	Origin        string `json:"origin,omitempty"`        // "discord" | "web" — where the run was STARTED, not whether it has a thread
	CreatedBy     string `json:"createdBy,omitempty"`     // Discord snowflake or web:<id>
	CreatedByName string `json:"createdByName,omitempty"` // display name
	DiscordURL    string `json:"discordUrl,omitempty"`    // legacy jump link; mirrored with Discord.URL

	// Discord holds this unit's Discord coordinates, nil for a web-native unit.
	// Its presence is the surface predicate — see HasDiscord. Populated by the
	// store, so callers never derive a surface from the shape of a unit id.
	Discord *DiscordRef `json:"discord,omitempty"`

	// Continuity / brief card: one pinned message (goal, progress, branch, PR, files).
	// Goal is sticky (first task prompt unless set via /brief goal …).
	Goal       string `json:"goal,omitempty"`
	BriefMsgID string `json:"briefMsgId,omitempty"`

	// Lifecycle label: open → in_progress → blocked → needs_review → done | abandoned.
	// Empty means open. LabelManual pauses auto updates until /label auto (terminal PR states still apply).
	Label       string `json:"label,omitempty"`
	LabelManual bool   `json:"labelManual,omitempty"`

	// Issues tracks GitHub issues/tickets bound to this thread (#N, URL, /link).
	// Used for PR body Fixes/Refs lines and title prefixes.
	Issues []TrackedIssue `json:"issues,omitempty"`

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

	// ShipMode is sticky per thread: "" (unset), "pr", or "direct" (No-PR / direct-to-primary).
	// Stamped on first run from project config; later runs honor the stamp.
	ShipMode      string `json:"shipMode,omitempty"`
	ShippedSHA    string `json:"shippedSha,omitempty"`
	ShippedAt     string `json:"shippedAt,omitempty"` // RFC3339
	PrimaryBranch string `json:"primaryBranch,omitempty"`

	// Mode is the session run mode: "" (legacy fix), "investigate", "explain", "fix", "case".
	// Orthogonal to ShipMode (K27). Empty = eng fix default for capable actors.
	Mode string `json:"mode,omitempty"`

	// Agent and Model are pinned when the session is created and never change.
	// SessionID is issued by one CLI and is meaningless to the other, so the
	// agent cannot move; the model is pinned with it so a thread's turns stay
	// consistent and editing global config never alters a live session. Set from
	// global config, or from a builder's pick on a web dispatch — Discord has no
	// command for it.
	//
	// Agent empty on an entry that already has a SessionID is pre-agent data,
	// which ran on grok. Model empty means the CLI chooses.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`

	// Wave 3 case lifecycle (Mode=case). Phase drives RunPolicy ship gates.
	// intake | investigate | answered | fixing | shipping | closed
	Phase string `json:"phase,omitempty"`

	// CaseKey is the case's quotable identity ("WEBAPP-14"), assigned at intake
	// and never changed — references written into commits, PRs and other cases
	// have to keep resolving. Distinct from CustomerRef, which is *their* id for
	// the ticket (ZD-4821) and is neither ours to mint nor guaranteed present.
	// Empty on cases filed before keys existed; see internal/sessionstore/casekey.go.
	CaseKey string `json:"caseKey,omitempty"`
	// RelatedCases are other cases' keys this one points at — "same root cause
	// as WEBAPP-9". Stored one-way and rendered both ways (the back-reference is
	// derived), so neither case has to be edited to keep the pair in sync.
	RelatedCases []string `json:"relatedCases,omitempty"`

	Severity      string `json:"severity,omitempty"`      // low|medium|high|critical
	CustomerTitle string `json:"customerTitle,omitempty"` // short external-safe title
	CustomerRef   string `json:"customerRef,omitempty"`   // opaque external id
	ReporterID    string `json:"reporterId,omitempty"`
	ReporterName  string `json:"reporterName,omitempty"`
	IntakeSource  string `json:"intakeSource,omitempty"` // discord|web

	CaseMsgID           string `json:"caseMsgId,omitempty"`
	DossierMsgID        string `json:"dossierMsgId,omitempty"`
	CustomerUpdateMsgID string `json:"customerUpdateMsgId,omitempty"`

	Dossier        *Dossier `json:"dossier,omitempty"`
	CustomerUpdate string   `json:"customerUpdate,omitempty"`

	// SLA clocks (RFC3339, see sla.go). OpenedAt is when the case was filed and
	// ReopenedAt below starts a fresh round; FirstResponseAt is the first
	// customer-facing reply of the current round; AnsweredAt is the last time
	// the case was handed back to the customer, which is where the resolution
	// clock freezes while we wait on them.
	//
	// Only timestamps live here. The per-severity targets are project policy
	// (config.SLATarget) and breach state is computed at render time
	// (internal/bot/case_sla.go) — a stored flag would be wrong the moment a
	// deadline passed with no writer running.
	OpenedAt        string `json:"openedAt,omitempty"`
	FirstResponseAt string `json:"firstResponseAt,omitempty"`
	AnsweredAt      string `json:"answeredAt,omitempty"`

	Resolution     string `json:"resolution,omitempty"` // answered|fixed|duplicate|wontfix|escalated_external
	ResolutionNote string `json:"resolutionNote,omitempty"`
	ResolvedAt     string `json:"resolvedAt,omitempty"`
	ResolvedBy     string `json:"resolvedBy,omitempty"`
	EscalatedAt    string `json:"escalatedAt,omitempty"`
	EscalatedBy    string `json:"escalatedBy,omitempty"`
	// Engineer* is the engineer who owns an escalation, set when a builder-class
	// actor escalates and cleared when a support-side actor does.
	//
	// Deliberately separate from OwnerID. A case is owned from the moment it is
	// filed — by the support member who filed it — and OwnerID additionally gates
	// cancel/reset. So OwnerID can never answer "has an engineer picked this up",
	// and clearing it to mean "nobody yet" would strip the reporter of control over
	// their own case.
	EngineerID   string `json:"engineerId,omitempty"`
	EngineerName string `json:"engineerName,omitempty"`
	ReopenedAt   string `json:"reopenedAt,omitempty"`
	ReopenedBy   string `json:"reopenedBy,omitempty"`

	// Wave 2 IDE-free confidence.
	Checkpoints   []CheckpointMeta `json:"checkpoints,omitempty"`
	OpenQuestions []OpenQuestion   `json:"openQuestions,omitempty"`
	VerifyMsgID   string           `json:"verifyMsgId,omitempty"`
	// LastVerify is the most recent @Grok /verify (or web verify) result for the session UI.
	LastVerify *LastVerify `json:"lastVerify,omitempty"`

	// WatcherIDs are Discord snowflakes who opted into a one-shot @mention when a
	// Grok run on this thread completes or fails (@Grok /watch).
	WatcherIDs []string `json:"watcherIds,omitempty"`
}

// LastVerify is a compact pass/fail snapshot for the web session panel.
type LastVerify struct {
	Name     string `json:"name,omitempty"` // command name(s)
	OK       bool   `json:"ok,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"` // last/aggregate exit
	At       string `json:"at,omitempty"`       // RFC3339
	Summary  string `json:"summary,omitempty"`  // one-line e.g. "unit pass · 1.2s"
	LogTail  string `json:"logTail,omitempty"`  // capped log excerpt
}

// CheckpointMeta is bot-owned git checkpoint metadata (refs/grok-cp/<threadId>/<id>).
type CheckpointMeta struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"createdAt"`
	CreatedBy string `json:"createdBy,omitempty"`
}

// OpenQuestion is a decision card / brief open question.
type OpenQuestion struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Status  string `json:"status,omitempty"` // open|answered|dismissed
	AskedAt string `json:"askedAt,omitempty"`
	Answer  string `json:"answer,omitempty"`
	// Options are button labels when posted as a decision card (max ~5).
	Options []string `json:"options,omitempty"`
}

// Wave 2 size caps.
const (
	MaxCheckpoints   = 20
	MaxOpenQuestions = 15
)

// ClampWave2Fields enforces checkpoint/open-question caps. Always nil error.
func ClampWave2Fields(e *Entry) error {
	if e == nil {
		return nil
	}
	if len(e.Checkpoints) > MaxCheckpoints {
		// Drop oldest (front).
		e.Checkpoints = append([]CheckpointMeta(nil), e.Checkpoints[len(e.Checkpoints)-MaxCheckpoints:]...)
	}
	if len(e.OpenQuestions) > MaxOpenQuestions {
		e.OpenQuestions = append([]OpenQuestion(nil), e.OpenQuestions[len(e.OpenQuestions)-MaxOpenQuestions:]...)
	}
	return nil
}

// FindCheckpoint returns metadata for id (case-sensitive short id).
func (e Entry) FindCheckpoint(id string) (CheckpointMeta, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return CheckpointMeta{}, false
	}
	for _, c := range e.Checkpoints {
		if c.ID == id {
			return c, true
		}
	}
	return CheckpointMeta{}, false
}

// LatestCheckpoint returns the most recently appended checkpoint.
func (e Entry) LatestCheckpoint() (CheckpointMeta, bool) {
	if len(e.Checkpoints) == 0 {
		return CheckpointMeta{}, false
	}
	return e.Checkpoints[len(e.Checkpoints)-1], true
}

// Case phase constants.
const (
	PhaseIntake      = "intake"
	PhaseInvestigate = "investigate"
	PhaseAnswered    = "answered"
	PhaseFixing      = "fixing"
	PhaseShipping    = "shipping"
	PhaseClosed      = "closed"
)

// Dossier is the internal investigation artifact (support + eng).
type Dossier struct {
	Summary      string   `json:"summary,omitempty"`
	ReproSteps   []string `json:"reproSteps,omitempty"`
	Environment  string   `json:"environment,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	Hypotheses   []string `json:"hypotheses,omitempty"`
	KnownBugHits []string `json:"knownBugHits,omitempty"`
	NextActions  []string `json:"nextActions,omitempty"`
	UpdatedAt    string   `json:"updatedAt,omitempty"`
}

// IsCase reports Mode=case.
func (e Entry) IsCase() bool {
	return strings.EqualFold(strings.TrimSpace(e.Mode), "case")
}

// CasePhase returns normalized phase or empty.
func (e Entry) CasePhase() string {
	return strings.ToLower(strings.TrimSpace(e.Phase))
}

// IsCaseClosed is true when Mode=case and Phase=closed.
func (e Entry) IsCaseClosed() bool {
	return e.IsCase() && e.CasePhase() == PhaseClosed
}

// IsCaseShipPhase is true when case may open PRs / direct-ship (fixing|shipping).
func (e Entry) IsCaseShipPhase() bool {
	if !e.IsCase() {
		return false
	}
	switch e.CasePhase() {
	case PhaseFixing, PhaseShipping:
		return true
	default:
		return false
	}
}

// Ship mode values for Entry.ShipMode.
const (
	ShipModePR     = "pr"
	ShipModeDirect = "direct"
)

// IsDirectShip reports whether this session uses direct-to-primary shipping.
func (e Entry) IsDirectShip() bool {
	return strings.TrimSpace(e.ShipMode) == ShipModeDirect
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
	if err := json.Unmarshal(raw, &s.entries); err != nil {
		return err
	}
	// Migrate legacy entries in place: a key that is not a web unit id is a
	// Discord thread id, so give it a DiscordRef. Idempotent.
	for id, e := range s.entries {
		ensureDiscordRef(id, &e)
		s.entries[id] = e
	}
	return nil
}

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	// sessions.json is the system of record for every thread/session/PR link
	// the bot knows about: it is replaced durably, never written in place. See
	// atomicfile.Write for why (including the directory fsync nobody expects).
	return atomicfile.Write(s.filePath, raw, 0o600)
}

func (s *Store) Get(threadID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[threadID]
	if !ok {
		return Entry{}, false
	}
	// Hand back state the caller owns outright — see Entry.clone.
	return e.clone(), true
}

func (s *Store) Set(threadID string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Clone on the way IN as well: storing the caller's Entry verbatim would
	// leave the store sharing the caller's slices and pointers, so a caller
	// that keeps its copy and mutates it later would corrupt store state
	// without going through Patch.
	e = e.clone()
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	ensureDiscordRef(threadID, &e)
	s.entries[threadID] = e
	return s.save()
}

// Patch loads the entry, applies fn, and saves. Returns false if missing.
// UpdatedAt is always refreshed when the entry exists.
func (s *Store) Patch(threadID string, fn func(*Entry)) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.entries[threadID]
	if !ok {
		return Entry{}, false, nil
	}
	// Apply fn to a detached copy. Without this, an fn that appends to a slice
	// with spare capacity would write through into the stored entry's backing
	// array before the assignment below — invisible, and wrong if save() fails.
	e := stored.clone()
	fn(&e)
	e.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	ensureDiscordRef(threadID, &e)
	s.entries[threadID] = e
	if err := s.save(); err != nil {
		return Entry{}, true, err
	}
	// Hand back state the caller owns outright — see Entry.clone.
	return e.clone(), true, nil
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
		// Hand back state the caller owns outright — see Entry.clone.
		out = append(out, Listed{ThreadID: id, Entry: e.clone()})
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
