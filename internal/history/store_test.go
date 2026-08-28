package history

import (
	"io"
	"os"
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

func TestAppendFilesOpenAndDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	a := filepath.Join(srcDir, "shot.png")
	b := filepath.Join(srcDir, "notes.txt")
	dup := filepath.Join(srcDir, "other.png")
	if err := os.WriteFile(a, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup, []byte("png-2"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.AppendFiles("t1", Turn{Prompt: "see shot", Status: "done", Project: "app"}, []File{
		{Path: a, Name: "shot.png", ContentType: "image/png"},
		{Path: b, Name: "notes.txt", ContentType: "text/plain"},
		{Path: dup, Name: "shot.png", ContentType: "image/png"},
		{Path: filepath.Join(srcDir, "missing.bin"), Name: "missing.bin"},
	}); err != nil {
		t.Fatal(err)
	}
	th, err := s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 1 {
		t.Fatalf("turns=%d", len(th.Turns))
	}
	atts := th.Turns[0].Attachments
	if len(atts) != 3 {
		t.Fatalf("attachments=%+v", atts)
	}
	if atts[0].Name != "shot.png" || atts[0].Size != 9 || atts[0].ContentType != "image/png" {
		t.Fatalf("first=%+v", atts[0])
	}
	if atts[1].Name != "notes.txt" || atts[1].Size != 5 {
		t.Fatalf("second=%+v", atts[1])
	}
	if atts[2].Name != "shot_2.png" {
		t.Fatalf("dup name=%q", atts[2].Name)
	}
	if !atts[0].IsImage() || atts[1].IsImage() {
		t.Fatalf("IsImage png=%v txt=%v", atts[0].IsImage(), atts[1].IsImage())
	}
	if atts[1].SizeLabel() != "5 B" {
		t.Fatalf("size label=%q", atts[1].SizeLabel())
	}

	f, meta, err := s.OpenFile("t1", 1, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "png-bytes" || meta.Name != "shot.png" {
		t.Fatalf("open=%q meta=%+v", raw, meta)
	}

	if _, _, err := s.OpenFile("t1", 1, "../shot.png"); err != nil {
		// filepath.Base collapses traversal; the allowlist then admits shot.png.
		t.Fatalf("collapsed name should open shot.png: %v", err)
	}
	if _, _, err := s.OpenFile("t1", 1, "missing.bin"); err == nil {
		t.Fatal("unlisted name must not open")
	}
	if _, _, err := s.OpenFile("t1", 2, "shot.png"); err == nil {
		t.Fatal("turn 2 must not exist")
	}
	if _, _, err := s.OpenFile("../etc", 1, "shot.png"); err == nil {
		t.Fatal("invalid thread id")
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ThreadID != "t1" {
		t.Fatalf("list must skip the files dir: %+v", list)
	}

	if err := s.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	th, err = s.Get("t1")
	if err != nil || len(th.Turns) != 0 {
		t.Fatalf("deleted get: %+v %v", th, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "history", "t1")); !os.IsNotExist(err) {
		t.Fatalf("files dir still present: %v", err)
	}
}

func TestAttachmentIsImage(t *testing.T) {
	if !(Attachment{Name: "a.png", ContentType: "image/png"}).IsImage() {
		t.Fatal("png")
	}
	if (Attachment{Name: "a.svg", ContentType: "image/svg+xml"}).IsImage() {
		t.Fatal("svg must not inline")
	}
	if !(Attachment{Name: "a.PNG"}).IsImage() {
		t.Fatal("ext fallback")
	}
	if (Attachment{Name: "a.html", ContentType: "text/html"}).IsImage() {
		t.Fatal("html")
	}
}

func TestDisplayError(t *testing.T) {
	if got := (Turn{Status: "done"}).DisplayError(); got != "" {
		t.Fatalf("done: %q", got)
	}
	if got := (Turn{Status: "error", Error: "Reached max turns before a final reply"}).DisplayError(); got != "Reached max turns before a final reply" {
		t.Fatalf("stored error: %q", got)
	}
	if got := (Turn{Status: "error", ExitCode: 1}).DisplayError(); got != "Grok exited with code 1" {
		t.Fatalf("legacy exit: %q", got)
	}
	if got := (Turn{Status: "cancelled"}).DisplayError(); got != "Cancelled" {
		t.Fatalf("cancelled: %q", got)
	}
}
