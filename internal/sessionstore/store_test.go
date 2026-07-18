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
