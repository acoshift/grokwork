package sessionstore

import (
	"path/filepath"
	"testing"
)

func TestListAndCount(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatalf("Count=%d", s.Count())
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("List=%v", list)
	}

	if err := s.Set("t2", Entry{SessionID: "s2", Project: "p", LastUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "q", LastUser: "bob"}); err != nil {
		t.Fatal(err)
	}

	if s.Count() != 2 {
		t.Fatalf("Count=%d want 2", s.Count())
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List len=%d", len(list))
	}
	// Newest (last Set) first.
	if list[0].ThreadID != "t1" || list[0].SessionID != "s1" || list[0].Project != "q" {
		t.Fatalf("first listed = %+v", list[0])
	}
	if list[1].ThreadID != "t2" {
		t.Fatalf("second listed = %+v", list[1])
	}
	if list[0].UpdatedAt == "" || list[0].LastUser != "bob" {
		t.Fatalf("entry fields: %+v", list[0])
	}

	// Reload from disk via new store on same data dir.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 2 {
		t.Fatalf("reloaded Count=%d", s2.Count())
	}
	// sessions.json path is under data dir.
	if _, err := filepath.Glob(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipHelpers(t *testing.T) {
	var e Entry
	if e.HasOwner() || e.CanControl("u1") {
		t.Fatal("empty entry should be unowned")
	}
	e.SetOwner("u1", "alice")
	if !e.HasOwner() || !e.IsOwner("u1") || e.OwnerName != "alice" {
		t.Fatalf("SetOwner: %+v", e)
	}
	e.AddCoOwner("u2")
	e.AddCoOwner("u2") // dedupe
	e.AddCoOwner("u1") // no-op (owner)
	if !e.IsCoOwner("u2") || e.IsCoOwner("u1") || len(e.CoOwnerIDs) != 1 {
		t.Fatalf("co-owners: %+v", e.CoOwnerIDs)
	}
	if !e.CanControl("u1") || !e.CanControl("u2") || e.CanControl("u3") {
		t.Fatalf("CanControl: owner=%v co=%v other=%v", e.CanControl("u1"), e.CanControl("u2"), e.CanControl("u3"))
	}

	e.HandOff("u3", "carol")
	if e.OwnerID != "u3" || e.OwnerName != "carol" {
		t.Fatalf("HandOff owner: %+v", e)
	}
	if !e.IsCoOwner("u1") || !e.IsCoOwner("u2") {
		t.Fatalf("HandOff co-owners: %+v", e.CoOwnerIDs)
	}
	// Claim-style SetOwner removes claimer from co-owners.
	e.SetOwner("u2", "bob")
	if e.IsCoOwner("u2") || e.OwnerID != "u2" {
		t.Fatalf("SetOwner clears co-owner slot: %+v", e)
	}
}

func TestPatchPRFields(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{SessionID: "s1", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Patch("t1", func(ent *Entry) {
		ent.PRNumber = 42
		ent.PRURL = "https://github.com/o/r/pull/42"
		ent.PRState = "OPEN"
		ent.PRStatusMsgID = "m1"
	})
	if err != nil || !ok {
		t.Fatalf("Patch: ok=%v err=%v", ok, err)
	}
	if e.PRNumber != 42 || e.SessionID != "s1" || e.PRStatusMsgID != "m1" {
		t.Fatalf("patched=%+v", e)
	}
	got, ok := s.Get("t1")
	if !ok || got.PRNumber != 42 {
		t.Fatalf("Get=%+v ok=%v", got, ok)
	}
	if _, ok, err := s.Patch("missing", func(*Entry) {}); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}
