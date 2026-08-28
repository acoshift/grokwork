package inbox

import (
	"testing"
)

func TestRewriteActorMergesByTime(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("42424", Item{Subject: "canonical-old", At: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("42424", Item{Subject: "canonical-new", At: "2026-01-03T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("github:999", Item{Subject: "alias-mid", At: "2026-01-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	n, err := st.RewriteActor("github:999", "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("merged = %d, want 1", n)
	}
	items, err := st.List("42424")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	// Newest first: canonical-new, alias-mid, canonical-old.
	got := []string{items[0].Subject, items[1].Subject, items[2].Subject}
	want := []string{"canonical-new", "alias-mid", "canonical-old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (alias must not look newest)", got, want)
		}
	}
	if items[0].Seq != 3 || items[1].Seq != 2 || items[2].Seq != 1 {
		t.Fatalf("seqs = %d,%d,%d want 3,2,1 newest-first", items[0].Seq, items[1].Seq, items[2].Seq)
	}
	alias, err := st.List("github:999")
	if err != nil {
		t.Fatal(err)
	}
	if len(alias) != 0 {
		t.Fatalf("alias feed still has %d items", len(alias))
	}
}

func TestRewriteActorPreservesCanonicalRead(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("42424", Item{Subject: "old", At: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("42424", Item{Subject: "newer", At: "2026-01-03T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAllRead("42424"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("github:999", Item{Subject: "from-alias", At: "2026-01-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RewriteActor("github:999", "42424"); err != nil {
		t.Fatal(err)
	}
	if n := st.UnreadCount("42424"); n != 1 {
		t.Fatalf("unread after absorb = %d, want 1 (the alias item)", n)
	}
	c := st.ReadCursor("42424")
	items, _ := st.List("42424")
	var aliasSeq int64
	for _, it := range items {
		if it.Subject == "from-alias" {
			aliasSeq = it.Seq
		}
	}
	if aliasSeq == 0 || !c.Unread(aliasSeq) {
		t.Fatalf("alias item seq=%d should be unread: %+v items=%+v", aliasSeq, c, items)
	}
}

func TestRewriteActorIdempotent(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("github:999", Item{Subject: "only"}); err != nil {
		t.Fatal(err)
	}
	n, err := st.RewriteActor("github:999", "42424")
	if err != nil || n != 1 {
		t.Fatalf("first merge n=%d err=%v", n, err)
	}
	n, err = st.RewriteActor("github:999", "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second merge n=%d, want 0", n)
	}
	items, _ := st.List("42424")
	if len(items) != 1 || items[0].Subject != "only" {
		t.Fatalf("after second merge: %+v", items)
	}
}

func TestRewriteActorSelfIsNoOp(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("42424", Item{Subject: "x"}); err != nil {
		t.Fatal(err)
	}
	n, err := st.RewriteActor("42424", "42424")
	if err != nil || n != 0 {
		t.Fatalf("self rewrite n=%d err=%v", n, err)
	}
	if st.Count("42424") != 1 {
		t.Fatal("self rewrite must not duplicate")
	}
}

func TestRewriteActorDedupsAlreadyMerged(t *testing.T) {
	st := newStore(t)
	if _, err := st.Append("42424", Item{Subject: "already", At: "2026-01-02T00:00:00Z", Kind: KindRunDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append("github:999", Item{Subject: "already", At: "2026-01-02T00:00:00Z", Kind: KindRunDone}); err != nil {
		t.Fatal(err)
	}
	n, err := st.RewriteActor("github:999", "42424")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("merged=%d want 0 (same item already on the account)", n)
	}
	if st.Count("42424") != 1 {
		t.Fatalf("duplicated: %d", st.Count("42424"))
	}
}

func TestRewriteActorEmptyFrom(t *testing.T) {
	st := newStore(t)
	if _, err := st.RewriteActor("", "42424"); err == nil {
		t.Fatal("empty from should be refused")
	}
}
