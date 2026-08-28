package inbox

import (
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAppendAndListNewestFirst(t *testing.T) {
	st := newStore(t)
	for _, s := range []string{"first", "second", "third"} {
		if _, err := st.Append("oidc:alice", Item{Kind: "run.done", Subject: s}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := st.List("oidc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].Subject != "third" {
		t.Errorf("first row = %q, want the newest (an inbox reads from the top)", items[0].Subject)
	}
	if items[0].Seq != 3 {
		t.Errorf("seq = %d, want 3", items[0].Seq)
	}
	if items[0].At == "" {
		t.Error("At not stamped")
	}
}

func TestFeedsAreIsolatedPerActor(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("oidc:alice", Item{Subject: "for alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("discord:123", Item{Subject: "for bob"}); err != nil {
		t.Fatal(err)
	}
	a, _ := st.List("oidc:alice")
	if len(a) != 1 || a[0].Subject != "for alice" {
		t.Fatalf("alice feed = %+v", a)
	}
	if n := st.Count("discord:123"); n != 1 {
		t.Errorf("bob count = %d, want 1", n)
	}
	if n := st.Count("oidc:nobody"); n != 0 {
		t.Errorf("unknown actor count = %d, want 0", n)
	}
}

// TestActorIDsWithColonsDoNotEscape guards the filesystem boundary: actor ids are
// namespaced, so ":" reaches this store on every call, and a traversal attempt
// must be refused rather than written to a neighbouring feed.
func TestActorIDsWithColonsDoNotEscape(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("discord:123", Item{Subject: "ok"}); err != nil {
		t.Fatalf("a normal namespaced id must be accepted: %v", err)
	}
	for _, bad := range []string{
		"", "   ", "../escape", "a/b", "a\\b", "oidc:../x", strings.Repeat("x", 129),
	} {
		if _, err := st.Append(bad, Item{Subject: "x"}); err == nil {
			t.Errorf("Append(%q) should be refused", bad)
		}
	}
	// Two distinct ids must not collide onto one file just because of sanitizing.
	if _, err := st.Append("oidc:a", Item{Subject: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := st.Count("oidc_a"); got != 0 {
		t.Errorf("oidc_a sees %d items from oidc:a — sanitize collision", got)
	}
}

func TestEmptySubjectRejected(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("oidc:alice", Item{Body: "body only"}); err == nil {
		t.Error("an item with no subject is not readable in a list; reject it")
	}
}

func TestOversizeBodyTruncated(t *testing.T) {
	st := newStore(t)
	it, err := st.Append("oidc:alice", Item{Subject: "s", Body: strings.Repeat("x", maxBodyBytes+100)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(it.Body, "(truncated)") {
		t.Error("oversize body should be truncated, not stored whole")
	}
}

func TestKindConstants(t *testing.T) {
	if KindRunDone != "run.done" || KindReviewRequested != "review.requested" {
		t.Fatalf("kind constants drifted: %q %q", KindRunDone, KindReviewRequested)
	}
}

func TestSeqSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("oidc:alice", Item{Subject: "a"}); err != nil {
		t.Fatal(err)
	}
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	it, err := st2.Append("oidc:alice", Item{Subject: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Seq != 2 {
		t.Errorf("seq after restart = %d, want 2", it.Seq)
	}
}
