package reviewstore

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSubmitReviewAndAsymmetricStale(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	r, err := s.SubmitReview(Review{
		Owner: "acme", Repo: "app", Number: 1,
		HeadSHA: "aaa", Verdict: VerdictApproved,
		ReviewerID: "u1", ReviewerName: "Alice",
		Body: "lgtm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "" {
		t.Fatal("expected id")
	}

	// Bob requests changes on same head.
	if _, err := s.SubmitReview(Review{
		Owner: "acme", Repo: "app", Number: 1,
		HeadSHA: "aaa", Verdict: VerdictChangesRequested,
		ReviewerID: "u2", Body: "fix nil",
	}); err != nil {
		t.Fatal(err)
	}

	b := s.ListForPR("acme", "app", 1)
	label, _, _ := TeamRollup(b, "aaa")
	if label != RollupChangesRequested {
		t.Fatalf("want CR on fresh head, got %s", label)
	}

	// Head advances: Alice approve goes stale; Bob CR stays sticky.
	label, _, ers := TeamRollup(b, "bbb")
	if label != RollupChangesRequested {
		t.Fatalf("CR must stay sticky after push, got %s", label)
	}
	var alice, bob *EffectiveReview
	for i := range ers {
		switch ers[i].ReviewerID {
		case "u1":
			alice = &ers[i]
		case "u2":
			bob = &ers[i]
		}
	}
	if alice == nil || !alice.Stale || alice.Current {
		t.Fatalf("alice approve should be stale and non-current: %+v", alice)
	}
	if bob == nil || !bob.Stale || !bob.Current {
		t.Fatalf("bob CR should be stale-but-current: %+v", bob)
	}

	// Bob re-approves on new head → Approved.
	if _, err := s.SubmitReview(Review{
		Owner: "acme", Repo: "app", Number: 1,
		HeadSHA: "bbb", Verdict: VerdictApproved,
		ReviewerID: "u2",
	}); err != nil {
		t.Fatal(err)
	}
	b = s.ListForPR("acme", "app", 1)
	label, _, _ = TeamRollup(b, "bbb")
	if label != RollupApproved {
		t.Fatalf("want approved after re-review, got %s", label)
	}
}

func TestCommentOnlyDoesNotCompleteRequestOrClearCR(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	req, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 3,
		RequesterID: "owner", ReviewerID: "rev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != StatusPending {
		t.Fatal(req.Status)
	}

	if _, err := s.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 3,
		HeadSHA: "h1", Verdict: VerdictChangesRequested,
		ReviewerID: "rev", Body: "please fix",
	}); err != nil {
		t.Fatal(err)
	}
	b := s.ListForPR("o", "r", 3)
	if b.Requests[0].Status != StatusCompleted {
		t.Fatalf("CR should complete request, got %s", b.Requests[0].Status)
	}

	// New pending after re-request.
	if _, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 3,
		RequesterID: "owner", ReviewerID: "rev",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 3,
		HeadSHA: "h2", Verdict: VerdictCommented,
		ReviewerID: "rev", Body: "drive-by note",
	}); err != nil {
		t.Fatal(err)
	}
	b = s.ListForPR("o", "r", 3)
	var pending int
	for _, r := range b.Requests {
		if r.Status == StatusPending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("comment-only must not complete request, pending=%d", pending)
	}

	// Latest effective still CR (comment does not clear).
	label, _, _ := TeamRollup(b, "h2")
	if label != RollupChangesRequested {
		t.Fatalf("comment must not clear CR, got %s", label)
	}
}

func TestObsoletePendingForPR(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 9,
		RequesterID: "a", ReviewerID: "b", HeadSHA: "old",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ObsoletePendingForPR("o", "r", 9, "MERGED", "final")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b := s.ListForPR("o", "r", 9)
	if b.LastState != "MERGED" || b.LastHeadSHA != "final" {
		t.Fatalf("stamp: %+v", b)
	}
	if b.Requests[0].Status != StatusObsolete {
		t.Fatal(b.Requests[0].Status)
	}
	if s.CountPendingForReviewer("b", "") != 0 {
		t.Fatal("pending should be gone")
	}
}

func TestListForReviewerProjectFilter(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 1, Project: "alpha",
		RequesterID: "a", ReviewerID: "me",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 2, Project: "beta",
		RequesterID: "a", ReviewerID: "me",
	}); err != nil {
		t.Fatal(err)
	}
	got := s.ListForReviewer("me", "alpha", StatusPending)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 1,
		HeadSHA: "x", Verdict: VerdictApproved, ReviewerID: "u",
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := s2.ListForPR("o", "r", 1)
	if len(b.Reviews) != 1 {
		t.Fatal(filepath.Join(dir, "pr-reviews.json"), b)
	}
}

func TestStaleApprovalsOnlyRollup(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 1,
		HeadSHA: "old", Verdict: VerdictApproved, ReviewerID: "u",
	}); err != nil {
		t.Fatal(err)
	}
	label, pending, _ := TeamRollup(s.ListForPR("o", "r", 1), "new")
	if label != RollupStaleApprovals || pending != 0 {
		t.Fatalf("got %s pending=%d", label, pending)
	}
}

func TestCancelRequestAuth(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.RequestReview(Request{
		Owner: "o", Repo: "r", Number: 1,
		RequesterID: "req", ReviewerID: "rev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelRequest("o", "r", 1, req.ID, "stranger"); err == nil {
		t.Fatal("stranger should fail")
	}
	if _, ok, err := s.CancelRequest("o", "r", 1, req.ID, "req"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestTouchPRHeadOnlyWhenActivity(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// No activity → no bucket created.
	if err := s.TouchPRHead("o", "r", 1, "abc", "OPEN"); err != nil {
		t.Fatal(err)
	}
	if b := s.ListForPR("o", "r", 1); b.LastHeadSHA != "" {
		t.Fatalf("unexpected bucket: %+v", b)
	}
	if _, err := s.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 1,
		HeadSHA: "old", Verdict: VerdictApproved, ReviewerID: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchPRHead("o", "r", 1, "new", "OPEN"); err != nil {
		t.Fatal(err)
	}
	b := s.ListForPR("o", "r", 1)
	if b.LastHeadSHA != "new" || b.LastState != "OPEN" {
		t.Fatalf("%+v", b)
	}
}

func TestChangesRequestedRequiresBody(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReview(Review{
		Owner: "o", Repo: "r", Number: 1,
		Verdict: VerdictChangesRequested, ReviewerID: "u",
	}); err == nil {
		t.Fatal("expected body required")
	}
}
