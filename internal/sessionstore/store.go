package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	// Dual-surface workflow metadata (web + Discord). Preserved across session Set rebuilds.
	Origin        string `json:"origin,omitempty"`        // "discord" | "web"
	CreatedBy     string `json:"createdBy,omitempty"`     // Discord snowflake or web:<id>
	CreatedByName string `json:"createdByName,omitempty"` // display name
	DiscordURL    string `json:"discordUrl,omitempty"`    // jump link when known

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

	// Wave 3 case lifecycle (Mode=case). Phase drives RunPolicy ship gates.
	// intake | investigate | answered | fixing | shipping | closed
	Phase string `json:"phase,omitempty"`

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

	Resolution     string `json:"resolution,omitempty"` // answered|fixed|duplicate|wontfix|escalated_external
	ResolutionNote string `json:"resolutionNote,omitempty"`
	ResolvedAt     string `json:"resolvedAt,omitempty"`
	ResolvedBy     string `json:"resolvedBy,omitempty"`
	EscalatedAt    string `json:"escalatedAt,omitempty"`
	EscalatedBy    string `json:"escalatedBy,omitempty"`
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
