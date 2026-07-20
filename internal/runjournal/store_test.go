package runjournal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadListDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := Journal{
		ThreadID:  "thread-1",
		SessionID: "sess-abc",
		Active: &TaskRecord{
			ID:               "task-1",
			Status:           StatusRunning,
			Prompt:           "fix the bug",
			Project:          "api",
			Source:           "discord",
			Actor:            Actor{ID: "u1", DisplayName: "alice"},
			Attempt:          1,
			AttachmentPaths:  []string{filepath.Join(s.TaskFilesDir("thread-1", "task-1"), "a.txt")},
			ReferencedPrompt: "re: earlier message",
			CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		},
		Queue: []TaskRecord{{
			ID: "task-2", Status: StatusPending, Prompt: "add tests", Project: "api",
			Source: "discord", Attempt: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}},
		GrokPID: 12345, Generation: 7, Host: "host-a",
	}
	if err := s.Save(&j); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load("thread-1")
	if err != nil || !ok || got.Active == nil || got.Active.Prompt != "fix the bug" {
		t.Fatalf("load: ok=%v err=%v active=%+v", ok, err, got.Active)
	}
	if !s.HasWork("thread-1") {
		t.Fatal("HasWork")
	}
	if list, err := s.List(); err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if err := s.Delete("thread-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load("thread-1"); ok {
		t.Fatal("expected missing")
	}
}

func TestUpdateIsAtomicRMW(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Journal{ThreadID: "t", Active: &TaskRecord{ID: "a", Status: StatusRunning, Attempt: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("t", func(j *Journal) error {
		j.Queue = append(j.Queue, TaskRecord{ID: "q1", Status: StatusPending, Prompt: "follow", Attempt: 1})
		j.GrokPID = 42
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Load("t")
	if !ok || got.GrokPID != 42 || len(got.Queue) != 1 || got.Queue[0].Prompt != "follow" {
		t.Fatalf("update: %+v", got)
	}
}

func TestTryLockAdoptsDeadPID(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	dead := LockFile{PID: 999999, StartedAt: time.Now().UTC().Format(time.RFC3339), Host: "h"}
	b, _ := json.MarshalIndent(dead, "", "  ")
	if err := os.WriteFile(filepath.Join(s.Dir(), ".lock"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.TryLock(os.Getpid(), time.Now(), "h"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	_ = s.Unlock(os.Getpid())
}

func TestRemoveTaskFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	td := s.TaskFilesDir("t1", "task-x")
	if err := os.MkdirAll(td, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(td, "f"), []byte("1"), 0o600)
	s.RemoveTaskFiles("t1", "task-x")
	if _, err := os.Stat(td); !os.IsNotExist(err) {
		t.Fatalf("expected removed: %v", err)
	}
}
