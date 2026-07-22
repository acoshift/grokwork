// Package reviewstore persists team PR reviews and review requests
// (Discord-attributed; independent of GitHub branch protection).
package reviewstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Verdict is a submitted review outcome.
type Verdict string

const (
	VerdictApproved         Verdict = "approved"
	VerdictChangesRequested Verdict = "changes_requested"
	VerdictCommented        Verdict = "commented"
)

// Request status values.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusObsolete  = "obsolete" // PR went terminal before assignee finished
)

// Team rollup labels for ship board / UI.
const (
	RollupChangesRequested = "changes_requested"
	RollupApproved         = "approved"
	RollupReviewRequested  = "review_requested"
	RollupStaleApprovals   = "stale_approvals"
	RollupNone             = "none"
)

// Review is one person's judgment of a PR at a specific head SHA.
type Review struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	Repo         string    `json:"repo"`
	Number       int       `json:"number"`
	Project      string    `json:"project,omitempty"`
	ThreadID     string    `json:"threadId,omitempty"`
	HeadSHA      string    `json:"headSha"`
	Verdict      Verdict   `json:"verdict"`
	Body         string    `json:"body,omitempty"`
	ReviewerID   string    `json:"reviewerId"`
	ReviewerName string    `json:"reviewerName,omitempty"`
	At           time.Time `json:"at"`
	GHCommentURL string    `json:"ghCommentUrl,omitempty"`
	GHMirroredAt time.Time `json:"ghMirroredAt,omitempty"`
	GHMirrorErr  string    `json:"ghMirrorErr,omitempty"`
}

// Request asks a specific Discord user to review a PR.
type Request struct {
	ID            string     `json:"id"`
	Owner         string     `json:"owner"`
	Repo          string     `json:"repo"`
	Number        int        `json:"number"`
	Project       string     `json:"project,omitempty"`
	ThreadID      string     `json:"threadId,omitempty"`
	HeadSHA       string     `json:"headSha,omitempty"`
	RequesterID   string     `json:"requesterId"`
	RequesterName string     `json:"requesterName,omitempty"`
	ReviewerID    string     `json:"reviewerId"`
	ReviewerName  string     `json:"reviewerName,omitempty"`
	Note          string     `json:"note,omitempty"`
	Status        string     `json:"status"` // pending | completed | cancelled | obsolete
	CreatedAt     time.Time  `json:"createdAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	ReviewID      string     `json:"reviewId,omitempty"`
}

// PRBucket holds all local review state for owner/repo#n.
// LastHeadSHA / LastState survive session cleanup so My Reviews can still render.
type PRBucket struct {
	Reviews     []Review  `json:"reviews,omitempty"`
	Requests    []Request `json:"requests,omitempty"`
	LastHeadSHA string    `json:"lastHeadSha,omitempty"`
	LastState   string    `json:"lastState,omitempty"` // OPEN | MERGED | CLOSED
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// EffectiveReview is the latest approve/CR per reviewer, with asymmetric stale flags.
type EffectiveReview struct {
	Review
	Stale   bool // HeadSHA != current head
	Sticky  bool // CR that still blocks even when Stale
	Current bool // drives rollup (fresh approve or any sticky/fresh CR)
}

// Store is a single-file JSON map keyed by PR identity (owner/repo#n).
type Store struct {
	mu       sync.Mutex
	filePath string
	buckets  map[string]PRBucket
	now      func() time.Time
}

// New loads or creates data/pr-reviews.json under dataDir.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		filePath: filepath.Join(dataDir, "pr-reviews.json"),
		buckets:  map[string]PRBucket{},
		now:      time.Now,
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
	return json.Unmarshal(raw, &s.buckets)
}

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.buckets, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, raw, 0o600)
}

// PRKey builds a stable bucket key.
func PRKey(owner, repo string, number int) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	if owner == "" || repo == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// NormalizeVerdict returns a known verdict or empty if invalid.
func NormalizeVerdict(v string) Verdict {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(VerdictApproved), "approve":
		return VerdictApproved
	case string(VerdictChangesRequested), "request_changes", "changes":
		return VerdictChangesRequested
	case string(VerdictCommented), "comment":
		return VerdictCommented
	default:
		return ""
	}
}

// SubmitReview appends a review. Completes pending requests for the same reviewer
// only when verdict is approved or changes_requested (not comment-only).
func (s *Store) SubmitReview(r Review) (Review, error) {
	if s == nil {
		return Review{}, fmt.Errorf("nil review store")
	}
	r.Owner = strings.TrimSpace(r.Owner)
	r.Repo = strings.TrimSpace(r.Repo)
	r.ReviewerID = strings.TrimSpace(r.ReviewerID)
	r.Verdict = NormalizeVerdict(string(r.Verdict))
	if r.Owner == "" || r.Repo == "" || r.Number <= 0 {
		return Review{}, fmt.Errorf("invalid PR identity")
	}
	if r.ReviewerID == "" {
		return Review{}, fmt.Errorf("empty reviewer id")
	}
	if r.Verdict == "" {
		return Review{}, fmt.Errorf("invalid verdict")
	}
	if r.Verdict == VerdictChangesRequested && strings.TrimSpace(r.Body) == "" {
		return Review{}, fmt.Errorf("body required for changes requested")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := PRKey(r.Owner, r.Repo, r.Number)
	b := s.buckets[key]
	now := s.now().UTC()
	if r.ID == "" {
		r.ID = newID()
	}
	if r.At.IsZero() {
		r.At = now
	} else {
		r.At = r.At.UTC()
	}
	b.Reviews = append(b.Reviews, r)

	if r.Verdict == VerdictApproved || r.Verdict == VerdictChangesRequested {
		for i := range b.Requests {
			req := &b.Requests[i]
			if req.Status != StatusPending {
				continue
			}
			if strings.TrimSpace(req.ReviewerID) != r.ReviewerID {
				continue
			}
			req.Status = StatusCompleted
			t := r.At
			req.CompletedAt = &t
			req.ReviewID = r.ID
		}
	}

	if r.HeadSHA != "" {
		b.LastHeadSHA = r.HeadSHA
	}
	if b.LastState == "" {
		b.LastState = "OPEN"
	}
	b.UpdatedAt = now
	s.buckets[key] = b
	if err := s.save(); err != nil {
		return Review{}, err
	}
	return r, nil
}

// PatchReview updates an existing review by id (e.g. GH mirror metadata).
func (s *Store) PatchReview(owner, repo string, number int, reviewID string, fn func(*Review)) (Review, bool, error) {
	if s == nil || fn == nil {
		return Review{}, false, fmt.Errorf("invalid patch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := PRKey(owner, repo, number)
	b, ok := s.buckets[key]
	if !ok {
		return Review{}, false, nil
	}
	for i := range b.Reviews {
		if b.Reviews[i].ID != reviewID {
			continue
		}
		fn(&b.Reviews[i])
		b.UpdatedAt = s.now().UTC()
		s.buckets[key] = b
		if err := s.save(); err != nil {
			return Review{}, true, err
		}
		return b.Reviews[i], true, nil
	}
	return Review{}, false, nil
}

// RequestReview records a pending review request.
func (s *Store) RequestReview(req Request) (Request, error) {
	if s == nil {
		return Request{}, fmt.Errorf("nil review store")
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Repo = strings.TrimSpace(req.Repo)
	req.RequesterID = strings.TrimSpace(req.RequesterID)
	req.ReviewerID = strings.TrimSpace(req.ReviewerID)
	if req.Owner == "" || req.Repo == "" || req.Number <= 0 {
		return Request{}, fmt.Errorf("invalid PR identity")
	}
	if req.ReviewerID == "" {
		return Request{}, fmt.Errorf("empty reviewer id")
	}
	if req.RequesterID == "" {
		return Request{}, fmt.Errorf("empty requester id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := PRKey(req.Owner, req.Repo, req.Number)
	b := s.buckets[key]
	// Avoid duplicate pending for same reviewer.
	for _, existing := range b.Requests {
		if existing.Status == StatusPending && existing.ReviewerID == req.ReviewerID {
			return existing, nil
		}
	}
	now := s.now().UTC()
	if req.ID == "" {
		req.ID = newID()
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = now
	} else {
		req.CreatedAt = req.CreatedAt.UTC()
	}
	req.Status = StatusPending
	b.Requests = append(b.Requests, req)
	if req.HeadSHA != "" {
		b.LastHeadSHA = req.HeadSHA
	}
	if b.LastState == "" {
		b.LastState = "OPEN"
	}
	b.UpdatedAt = now
	s.buckets[key] = b
	if err := s.save(); err != nil {
		return Request{}, err
	}
	return req, nil
}

// CancelRequest sets status cancelled if pending. actorID must be requester, reviewer, or empty (admin).
// When actorID is empty, cancel is allowed (caller already authorized as admin).
func (s *Store) CancelRequest(owner, repo string, number int, requestID, actorID string) (Request, bool, error) {
	if s == nil {
		return Request{}, false, fmt.Errorf("nil review store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := PRKey(owner, repo, number)
	b, ok := s.buckets[key]
	if !ok {
		return Request{}, false, nil
	}
	actorID = strings.TrimSpace(actorID)
	for i := range b.Requests {
		req := &b.Requests[i]
		if req.ID != requestID {
			continue
		}
		if req.Status != StatusPending {
			return *req, false, nil
		}
		if actorID != "" && actorID != req.RequesterID && actorID != req.ReviewerID {
			return Request{}, false, fmt.Errorf("not allowed to cancel")
		}
		req.Status = StatusCancelled
		t := s.now().UTC()
		req.CompletedAt = &t
		b.UpdatedAt = t
		s.buckets[key] = b
		if err := s.save(); err != nil {
			return Request{}, true, err
		}
		return *req, true, nil
	}
	return Request{}, false, nil
}

// TouchPRHead updates LastHeadSHA/LastState without mutating reviews.
// No-op when the PR has never had team reviews/requests (avoids polluting
// pr-reviews.json for every open PR the poller sees).
func (s *Store) TouchPRHead(owner, repo string, number int, headSHA, state string) error {
	if s == nil {
		return nil
	}
	key := PRKey(owner, repo, number)
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, exists := s.buckets[key]
	if !exists || (len(b.Reviews) == 0 && len(b.Requests) == 0) {
		return nil
	}
	changed := false
	if h := strings.TrimSpace(headSHA); h != "" && h != b.LastHeadSHA {
		b.LastHeadSHA = h
		changed = true
	}
	if st := strings.ToUpper(strings.TrimSpace(state)); st != "" && st != b.LastState {
		b.LastState = st
		changed = true
	}
	if !changed {
		return nil
	}
	b.UpdatedAt = s.now().UTC()
	s.buckets[key] = b
	return s.save()
}

// ObsoletePendingForPR marks all pending requests obsolete and stamps final state/head.
func (s *Store) ObsoletePendingForPR(owner, repo string, number int, finalState, finalHead string) (int, error) {
	if s == nil {
		return 0, nil
	}
	key := PRKey(owner, repo, number)
	if key == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[key]
	now := s.now().UTC()
	n := 0
	for i := range b.Requests {
		if b.Requests[i].Status != StatusPending {
			continue
		}
		b.Requests[i].Status = StatusObsolete
		t := now
		b.Requests[i].CompletedAt = &t
		n++
	}
	if h := strings.TrimSpace(finalHead); h != "" {
		b.LastHeadSHA = h
	}
	if st := strings.ToUpper(strings.TrimSpace(finalState)); st != "" {
		b.LastState = st
	}
	if n == 0 && len(b.Reviews) == 0 && len(b.Requests) == 0 && b.LastHeadSHA == "" && b.LastState == "" {
		return 0, nil
	}
	b.UpdatedAt = now
	s.buckets[key] = b
	if err := s.save(); err != nil {
		return n, err
	}
	return n, nil
}

// ListForPR returns a copy of the bucket (empty if missing).
func (s *Store) ListForPR(owner, repo string, number int) PRBucket {
	if s == nil {
		return PRBucket{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[PRKey(owner, repo, number)]
	return cloneBucket(b)
}

// ListForReviewer returns requests for a Discord user, newest first.
// statusFilter: empty or "pending" (default when empty for queue UX callers should pass explicitly),
// "completed", "cancelled", "obsolete", "all".
func (s *Store) ListForReviewer(userID, projectFilter, statusFilter string) []Request {
	if s == nil {
		return nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	projectFilter = strings.TrimSpace(projectFilter)
	statusFilter = strings.ToLower(strings.TrimSpace(statusFilter))
	if statusFilter == "" {
		statusFilter = StatusPending
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Request
	for _, b := range s.buckets {
		for _, req := range b.Requests {
			if req.ReviewerID != userID {
				continue
			}
			if projectFilter != "" && !strings.EqualFold(req.Project, projectFilter) {
				continue
			}
			if statusFilter != "all" && req.Status != statusFilter {
				continue
			}
			out = append(out, req)
		}
	}
	slices.SortFunc(out, func(a, b Request) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return strings.Compare(b.ID, a.ID)
		}
		if a.CreatedAt.Before(b.CreatedAt) {
			return 1
		}
		return -1
	})
	return out
}

// CountPendingForReviewer returns pending request count (optional project filter).
func (s *Store) CountPendingForReviewer(userID, projectFilter string) int {
	return len(s.ListForReviewer(userID, projectFilter, StatusPending))
}

// LatestPerReviewer returns the latest approve/CR per reviewer (comment-only ignored for effective verdict).
func LatestPerReviewer(reviews []Review) []Review {
	// Newest first per reviewer among approve/CR only.
	type idx struct {
		r Review
		i int
	}
	best := map[string]idx{}
	for i, r := range reviews {
		if r.Verdict != VerdictApproved && r.Verdict != VerdictChangesRequested {
			continue
		}
		id := strings.TrimSpace(r.ReviewerID)
		if id == "" {
			continue
		}
		prev, ok := best[id]
		if !ok || r.At.After(prev.r.At) || (r.At.Equal(prev.r.At) && i > prev.i) {
			best[id] = idx{r: r, i: i}
		}
	}
	out := make([]Review, 0, len(best))
	for _, v := range best {
		out = append(out, v.r)
	}
	slices.SortFunc(out, func(a, b Review) int {
		if a.At.Equal(b.At) {
			return strings.Compare(a.ReviewerID, b.ReviewerID)
		}
		if a.At.Before(b.At) {
			return 1
		}
		return -1
	})
	return out
}

// EffectiveReviews applies asymmetric stale rules against currentHead.
// Approvals expire when head moves; CR stays sticky (blocking) when stale.
func EffectiveReviews(reviews []Review, currentHead string) []EffectiveReview {
	currentHead = strings.TrimSpace(currentHead)
	latest := LatestPerReviewer(reviews)
	out := make([]EffectiveReview, 0, len(latest))
	for _, r := range latest {
		stale := currentHead != "" && r.HeadSHA != "" && !shaEqual(r.HeadSHA, currentHead)
		er := EffectiveReview{Review: r, Stale: stale}
		switch r.Verdict {
		case VerdictChangesRequested:
			er.Sticky = stale // sticky when head moved; still blocking either way
			er.Current = true // always blocks
		case VerdictApproved:
			er.Current = !stale // only fresh approvals count
		}
		out = append(out, er)
	}
	return out
}

// TeamRollup computes ship-board team review status.
func TeamRollup(bucket PRBucket, currentHead string) (label string, pending int, effectives []EffectiveReview) {
	if currentHead == "" {
		currentHead = bucket.LastHeadSHA
	}
	effectives = EffectiveReviews(bucket.Reviews, currentHead)
	for _, req := range bucket.Requests {
		if req.Status == StatusPending {
			pending++
		}
	}

	hasStickyCR := false
	hasFreshApprove := false
	hasStaleApprove := false
	for _, er := range effectives {
		switch er.Verdict {
		case VerdictChangesRequested:
			hasStickyCR = true
		case VerdictApproved:
			if er.Stale {
				hasStaleApprove = true
			} else {
				hasFreshApprove = true
			}
		}
	}

	switch {
	case hasStickyCR:
		return RollupChangesRequested, pending, effectives
	case hasFreshApprove:
		return RollupApproved, pending, effectives
	case pending > 0:
		return RollupReviewRequested, pending, effectives
	case hasStaleApprove:
		return RollupStaleApprovals, pending, effectives
	default:
		return RollupNone, pending, effectives
	}
}

// IsReviewFresh reports whether a single review matches current head.
func IsReviewFresh(headSHA, currentHead string) bool {
	headSHA = strings.TrimSpace(headSHA)
	currentHead = strings.TrimSpace(currentHead)
	if headSHA == "" || currentHead == "" {
		return false
	}
	return shaEqual(headSHA, currentHead)
}

func shaEqual(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func cloneBucket(b PRBucket) PRBucket {
	out := b
	if b.Reviews != nil {
		out.Reviews = slices.Clone(b.Reviews)
	}
	if b.Requests != nil {
		out.Requests = slices.Clone(b.Requests)
	}
	return out
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
