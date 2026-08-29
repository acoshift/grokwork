package history

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestPutAndOpenArtifact(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "report.xlsx")
	if err := os.WriteFile(src, []byte("sheet"), 0o600); err != nil {
		t.Fatal(err)
	}

	att, err := s.PutArtifact("t1", File{
		Path: src, Name: "report.xlsx", ContentType: "application/vnd.ms-excel", Rel: "dist/report.xlsx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if att.Name != "report.xlsx" || att.Size != 5 || att.Rel != "dist/report.xlsx" {
		t.Fatalf("att=%+v", att)
	}

	dup, err := s.PutArtifact("t1", File{Bytes: []byte("sheet2"), Name: "report.xlsx", Rel: "/etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	if dup.Name != "report_2.xlsx" {
		t.Fatalf("dup name=%q", dup.Name)
	}
	if dup.Rel != "" {
		t.Fatalf("absolute rel must be dropped: %q", dup.Rel)
	}

	th, err := s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Artifacts) != 2 || len(th.Turns) != 0 {
		t.Fatalf("thread=%+v", th)
	}

	f, meta, err := s.OpenArtifact("t1", "report.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "sheet" || meta.Rel != "dist/report.xlsx" {
		t.Fatalf("open=%q meta=%+v", raw, meta)
	}

	if _, _, err := s.OpenArtifact("t1", "missing.bin"); err == nil {
		t.Fatal("unlisted name must not open")
	}
	if _, _, err := s.OpenArtifact("../t1", "report.xlsx"); err == nil {
		t.Fatal("invalid thread id")
	}

	if err := s.Append("t1", Turn{Prompt: "build", Status: "done", Artifacts: []Attachment{att}}); err != nil {
		t.Fatal(err)
	}
	th, err = s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Artifacts) != 2 {
		t.Fatalf("append must keep session artifacts: %+v", th.Artifacts)
	}
	if len(th.Turns[0].Artifacts) != 1 || th.Turns[0].Artifacts[0].Name != "report.xlsx" {
		t.Fatalf("turn artifacts=%+v", th.Turns[0].Artifacts)
	}

	third, err := s.PutArtifact("t1", File{Bytes: []byte("x"), Name: "report.xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Name != "report_3.xlsx" {
		t.Fatalf("must skip taken _2 suffix: %q", third.Name)
	}

	if err := s.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenArtifact("t1", "report.xlsx"); err == nil {
		t.Fatal("deleted artifact must not open")
	}
}

func TestPutArtifactDoesNotRewriteTurnLog(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("t1", Turn{Prompt: "do it", Response: "done", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "history", "t1.json")
	before, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), `"artifacts"`) {
		t.Fatal("turn log must not carry artifacts")
	}
	if _, err := s.PutArtifact("t1", File{Bytes: []byte("xlsx"), Name: "a.xlsx"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("PutArtifact must not rewrite the turn log")
	}
	th, err := s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 1 || th.Turns[0].Prompt != "do it" {
		t.Fatalf("turns clobbered: %+v", th.Turns)
	}
	if len(th.Artifacts) != 1 || th.Artifacts[0].Name != "a.xlsx" {
		t.Fatalf("sidecar not visible on Get: %+v", th.Artifacts)
	}
}

func TestLegacyJSONArtifactsMigrateOnSave(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{
  "threadId": "t1",
  "turns": [{"at": "2026-01-01T00:00:00Z", "prompt": "old", "status": "done"}],
  "artifacts": [{"name": "legacy.xlsx", "size": 3, "rel": "out/legacy.xlsx"}]
}
`)
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history", "t1.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	th, err := s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Artifacts) != 1 || th.Artifacts[0].Name != "legacy.xlsx" {
		t.Fatalf("legacy Get: %+v", th.Artifacts)
	}
	if err := s.Append("t1", Turn{Prompt: "next", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "history", "t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"artifacts"`) {
		t.Fatalf("migrated turn log still has artifacts: %s", raw)
	}
	th, err = s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Artifacts) != 1 || th.Artifacts[0].Name != "legacy.xlsx" {
		t.Fatalf("sidecar after migrate: %+v", th.Artifacts)
	}
	if len(th.Turns) != 2 {
		t.Fatalf("turns=%d", len(th.Turns))
	}
}

func TestSafeRelLabel(t *testing.T) {
	if got := safeRelLabel("dist/app.apk"); got != "dist/app.apk" {
		t.Fatalf("rel=%q", got)
	}
	if got := safeRelLabel("/etc/passwd"); got != "" {
		t.Fatalf("abs=%q", got)
	}
	if got := safeRelLabel("../secret"); got != "" {
		t.Fatalf("escape=%q", got)
	}
	if got := safeRelLabel(`C:\Windows\notepad.exe`); got != "" {
		t.Fatalf("drive=%q", got)
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
