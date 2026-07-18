package history

import (
	"path/filepath"
	"testing"
)

func TestAppendGetList(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Append("111", Turn{
		User: "alice", UserID: "1", Prompt: "fix bug", Response: "done",
		Status: "done", Project: "app", Elapsed: "3s",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("111", Turn{
		User: "bob", Prompt: "follow up", Response: "also done",
		Status: "done", Project: "app",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("222", Turn{
		User: "carol", Prompt: "other thread", Response: "ok",
		Status: "cancelled", Project: "api",
	}); err != nil {
		t.Fatal(err)
	}

	th, err := s.Get("111")
	if err != nil {
		t.Fatal(err)
	}
	if th.ThreadID != "111" || th.Project != "app" || len(th.Turns) != 2 {
		t.Fatalf("thread=%+v", th)
	}
	if th.Turns[0].Prompt != "fix bug" || th.Turns[1].Response != "also done" {
		t.Fatalf("turns=%+v", th.Turns)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list=%+v", list)
	}
	// Newest first: 222 was last written at end, but timestamps may be same-second;
	// both should appear with correct turn counts.
	byID := map[string]Summary{}
	for _, row := range list {
		byID[row.ThreadID] = row
	}
	if byID["111"].TurnCount != 2 || byID["111"].LastPrompt == "" {
		t.Fatalf("summary 111=%+v", byID["111"])
	}
	if byID["222"].TurnCount != 1 || byID["222"].LastStatus != "cancelled" {
		t.Fatalf("summary 222=%+v", byID["222"])
	}

	// Reload from disk.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	th2, err := s2.Get("111")
	if err != nil || len(th2.Turns) != 2 {
		t.Fatalf("reload: %+v %v", th2, err)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "history", "*.json")); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidThreadID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("../etc", Turn{Prompt: "x"}); err == nil {
		t.Fatal("expected invalid id error")
	}
	if err := s.Append("a/b", Turn{Prompt: "x"}); err == nil {
		t.Fatal("expected invalid id error")
	}
}
