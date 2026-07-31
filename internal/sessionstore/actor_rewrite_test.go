package sessionstore

import (
	"slices"
	"strings"
	"testing"
)

// sameAs is the caller-supplied spelling rule, standing in for
// config.SameActor: bare ids and "discord:"-prefixed ones are one actor.
func sameAs(want string) func(string) bool {
	norm := func(id string) string {
		id = strings.TrimSpace(id)
		if k, sub, found := strings.Cut(id, ":"); found && strings.EqualFold(k, "discord") {
			return sub
		}
		return id
	}
	return func(id string) bool { return id != "" && norm(id) == norm(want) }
}

func rewriteStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A unit owned by a login that is now an alias has an owner who can no longer
// cancel, claim or filter for it — the alias is never minted again.
func TestRewriteActorMovesEveryActorField(t *testing.T) {
	s := rewriteStore(t)
	if err := s.Set("t1", Entry{
		SessionID:   "s1",
		Project:     "app",
		OwnerID:     "github:999",
		CoOwnerIDs:  []string{"github:999", "someone"},
		WatcherIDs:  []string{"github:999"},
		CreatedBy:   "github:999",
		ReporterID:  "github:999",
		EngineerID:  "github:999",
		EscalatedBy: "github:999",
		ResolvedBy:  "github:999",
		ReopenedBy:  "github:999",
		Checkpoints: []CheckpointMeta{{ID: "c1", CreatedBy: "github:999"}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.RewriteActor(sameAs("github:999"), "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("rewrote %d fields, want 10", n)
	}
	e, _ := s.Get("t1")
	for name, got := range map[string]string{
		"OwnerID": e.OwnerID, "CreatedBy": e.CreatedBy, "ReporterID": e.ReporterID,
		"EngineerID": e.EngineerID, "EscalatedBy": e.EscalatedBy, "ResolvedBy": e.ResolvedBy,
		"ReopenedBy": e.ReopenedBy, "checkpoint.CreatedBy": e.Checkpoints[0].CreatedBy,
	} {
		if got != "42424" {
			t.Errorf("%s=%q want the account", name, got)
		}
	}
	if !slices.Equal(e.CoOwnerIDs, []string{"someone"}) {
		// The rewritten co-owner is now the owner, and SetOwner's invariant is
		// that the two lists never overlap.
		t.Errorf("coOwners=%v", e.CoOwnerIDs)
	}
	if !slices.Equal(e.WatcherIDs, []string{"42424"}) {
		t.Errorf("watchers=%v", e.WatcherIDs)
	}
}

// UpdatedAt is when work last happened. The terminal-session sweeper deletes on
// it, so stamping every unit a person owns on the day they link a login would
// hand every abandoned thread another full TTL — and reorder every board.
func TestRewriteActorKeepsUpdatedAt(t *testing.T) {
	s := rewriteStore(t)
	fixed := "2026-01-15T12:00:00Z"
	if err := s.Set("t1", Entry{SessionID: "s1", OwnerID: "github:999", UpdatedAt: fixed}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("t1")
	if before.UpdatedAt != fixed {
		t.Fatalf("fixture UpdatedAt=%q want %q", before.UpdatedAt, fixed)
	}
	if _, err := s.RewriteActor(sameAs("github:999"), "42424"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get("t1")
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("UpdatedAt moved %q → %q", before.UpdatedAt, after.UpdatedAt)
	}
}

// Repeating a link repeats the rewrite; it must find nothing the second time,
// and must not list one person twice the first time.
func TestRewriteActorIsIdempotentAndDoesNotDuplicate(t *testing.T) {
	s := rewriteStore(t)
	if err := s.Set("t1", Entry{
		SessionID:  "s1",
		OwnerID:    "keeper",
		CoOwnerIDs: []string{"42424", "github:999"},
		WatcherIDs: []string{"42424", "github:999"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewriteActor(sameAs("github:999"), "42424"); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get("t1")
	if !slices.Equal(e.CoOwnerIDs, []string{"42424"}) {
		t.Fatalf("coOwners=%v want one entry", e.CoOwnerIDs)
	}
	if !slices.Equal(e.WatcherIDs, []string{"42424"}) {
		t.Fatalf("watchers=%v want one entry", e.WatcherIDs)
	}
	n, err := s.RewriteActor(sameAs("github:999"), "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second pass rewrote %d fields", n)
	}
}

// Untouched units must survive byte-identical: absorbing one person's logins is
// not licence to rewrite the store.
func TestRewriteActorLeavesOtherUnitsAlone(t *testing.T) {
	s := rewriteStore(t)
	if err := s.Set("mine", Entry{SessionID: "s1", OwnerID: "github:999"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("theirs", Entry{SessionID: "s2", OwnerID: "stranger", WatcherIDs: []string{"nobody"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("theirs")
	if _, err := s.RewriteActor(sameAs("github:999"), "42424"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get("theirs")
	if after.OwnerID != before.OwnerID || after.UpdatedAt != before.UpdatedAt ||
		!slices.Equal(after.WatcherIDs, before.WatcherIDs) {
		t.Fatalf("unrelated unit changed: %+v → %+v", before, after)
	}
}

// It persists: a rewrite that only lives in memory is undone by the next boot.
func TestRewriteActorPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", OwnerID: "github:999"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewriteActor(sameAs("github:999"), "42424"); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := reopened.Get("t1")
	if !ok || e.OwnerID != "42424" {
		t.Fatalf("reloaded owner=%q ok=%v", e.OwnerID, ok)
	}
}
