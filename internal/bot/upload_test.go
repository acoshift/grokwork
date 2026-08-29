package bot

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParseUploadPaths(t *testing.T) {
	text := `
Built the release APK.

ARTIFACT:
- app/build/outputs/apk/release/app-release.apk
reports/results.xlsx

DISCORD_UPLOAD: single.pdf
`
	got := parseUploadPaths(text)
	want := []string{
		"app/build/outputs/apk/release/app-release.apk",
		"reports/results.xlsx",
		"single.pdf",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if n := len(parseUploadPaths("no markers")); n != 0 {
		t.Fatalf("expected empty, got %d", n)
	}
}

func TestResolveWorktreeUploadPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "dist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "app.apk")
	if err := os.WriteFile(file, []byte("apk"), 0o600); err != nil {
		t.Fatal(err)
	}

	abs, err := resolveWorktreeUploadPath(root, "dist/app.apk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("abs=%q: %v", abs, err)
	}

	abs2, err := resolveWorktreeUploadPath(root, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs2); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveWorktreeUploadPath(root, "../secret"); err == nil {
		t.Fatal("expected escape error")
	}
	if _, err := resolveWorktreeUploadPath(root, "/etc/passwd"); err == nil {
		t.Fatal("expected abs escape error")
	}
	if _, err := resolveWorktreeUploadPath(root, "missing.bin"); err == nil {
		t.Fatal("expected missing error")
	}
	if _, err := resolveWorktreeUploadPath(root, "dist"); err == nil {
		t.Fatal("expected directory error")
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink not supported:", err)
	}
	if _, err := resolveWorktreeUploadPath(root, "escape.txt"); err == nil {
		t.Fatal("expected symlink escape error")
	}
}

func TestPrepareWorktreeUploads(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.xlsx"), []byte("sheet"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, notes := prepareWorktreeUploads(root, []string{"ok.xlsx", "../nope", "missing.pdf"})
	if len(ok) != 1 || ok[0].Name != "ok.xlsx" {
		t.Fatalf("ok=%+v notes=%v", ok, notes)
	}
	if len(notes) < 2 {
		t.Fatalf("expected skip notes: %v", notes)
	}
}

func TestRemotePromptMentionsUploadWhenWorktree(t *testing.T) {
	with := remoteWorkPromptPrefix("grok/discord/1")
	if !strings.Contains(with, "ARTIFACT:") {
		t.Fatal("worktree prompt should document ARTIFACT")
	}
	if !strings.Contains(with, "DISCORD_UPLOAD:") {
		t.Fatal("worktree prompt should keep DISCORD_UPLOAD as an alias")
	}
	without := remoteWorkPromptPrefix("")
	if strings.Contains(without, "ARTIFACT:") {
		t.Fatal("non-worktree prompt should not advertise path markers")
	}
	if !strings.Contains(without, "session_send_file") {
		t.Fatal("non-worktree should mention content-based session_send_file")
	}
	inv := investigatePromptPrefix("grokwork/1", false)
	if !strings.Contains(inv, "ARTIFACT:") {
		t.Fatal("investigate with worktree should document ARTIFACT")
	}
	plan := planPromptPrefix("grokwork/1")
	if !strings.Contains(plan, "ARTIFACT:") {
		t.Fatal("plan with worktree should document ARTIFACT")
	}
}

func TestIngestWorktreeArtifactsPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{DataDir: dir}, store, hist)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.xlsx"), []byte("sheet"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := b.ingestWorktreeArtifacts("t1", root, "ARTIFACT:\nok.xlsx\n../secret\n")
	if len(notes) == 0 {
		t.Fatal("expected skip note for escape path")
	}
	f, meta, err := hist.OpenArtifact("t1", "ok.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "sheet" || meta.Name != "ok.xlsx" {
		t.Fatalf("open=%q meta=%+v", raw, meta)
	}
	th, err := hist.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Artifacts) != 1 {
		t.Fatalf("artifacts=%+v", th.Artifacts)
	}
}
