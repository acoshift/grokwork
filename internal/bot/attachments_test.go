package bot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"error.log":        "error.log",
		"../../etc/passwd": "passwd",
		"weird name!!.txt": "weird name__.txt",
		"":                 "file",
		".":                "file",
		"..":               "file",
		"a/b/c.png":        "c.png",
		"สวัสดี.txt":       "สวัสดี.txt",
		"ข้อมูลลูกค้า.xlsx":               "ข้อมูลลูกค้า.xlsx",
		"ที่อยู่จัดส่ง.csv":               "ที่อยู่จัดส่ง.csv",
		strings.Repeat("x", 200) + ".log": "", // length capped; checked below
	}
	for in, want := range cases {
		got := sanitizeFilename(in)
		if want == "" {
			if len(got) > 120 {
				t.Errorf("sanitizeFilename(%q) len=%d want <=120", in, len(got))
			}
			if !strings.HasSuffix(got, ".log") {
				t.Errorf("sanitizeFilename long name should keep .log, got %q", got)
			}
			continue
		}
		if got != want {
			t.Errorf("sanitizeFilename(%q)=%q want %q", in, got, want)
		}
	}
}

func TestUniqueFilename(t *testing.T) {
	used := map[string]int{}
	if g := uniqueFilename("a.txt", used); g != "a.txt" {
		t.Fatalf("first=%q", g)
	}
	if g := uniqueFilename("a.txt", used); g != "a_2.txt" {
		t.Fatalf("second=%q", g)
	}
}

func TestPromptWithAttachments(t *testing.T) {
	files := []savedAttachment{
		{Path: "/tmp/x/err.log", Filename: "err.log", ContentType: "text/plain", Size: 100},
		{Path: "/tmp/x/shot.png", Filename: "shot.png", ContentType: "image/png", Size: 2048},
	}
	got := promptWithAttachments("fix the crash", files)
	if !strings.Contains(got, "fix the crash") {
		t.Fatalf("missing user prompt: %q", got)
	}
	if !strings.Contains(got, "/tmp/x/err.log") || !strings.Contains(got, "/tmp/x/shot.png") {
		t.Fatalf("missing paths: %q", got)
	}
	if !strings.Contains(got, "text/plain") {
		t.Fatalf("missing content type: %q", got)
	}

	got = promptWithAttachments("", files)
	if !strings.HasPrefix(got, "Please review the attached files.") {
		t.Fatalf("empty prompt default: %q", got)
	}
	if promptWithAttachments("hi", nil) != "hi" {
		t.Fatal("no files should leave prompt unchanged")
	}
}

func TestDownloadAttachments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/log":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("boom\n"))
		case "/img":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	files, err := downloadAttachments(context.Background(), []*discordgo.MessageAttachment{
		{ID: "1", URL: srv.URL + "/log", Filename: "err.log", Size: 5, ContentType: "text/plain"},
		{ID: "2", URL: srv.URL + "/img", Filename: "shot.png", Size: 4, ContentType: "image/png"},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files", len(files))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "err.log"))
	if err != nil || string(raw) != "boom\n" {
		t.Fatalf("err.log: %q %v", raw, err)
	}
}

func TestDownloadAttachmentsThai(t *testing.T) {
	const body = "สวัสดีครับ ข้อมูลลูกค้า\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	files, err := downloadAttachments(context.Background(), []*discordgo.MessageAttachment{
		{ID: "1", URL: srv.URL + "/note", Filename: "ที่อยู่จัดส่ง.txt", Size: len(body), ContentType: "text/plain"},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files", len(files))
	}
	if files[0].Filename != "ที่อยู่จัดส่ง.txt" {
		t.Fatalf("filename=%q", files[0].Filename)
	}
	if !utf8.ValidString(files[0].Filename) {
		t.Fatal("filename is not valid utf-8")
	}
	raw, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("body=%q", raw)
	}
}

func TestDownloadAttachmentsTooMany(t *testing.T) {
	atts := make([]*discordgo.MessageAttachment, maxAttachments+1)
	for i := range atts {
		atts[i] = &discordgo.MessageAttachment{Filename: "f", Size: 1, URL: "http://example"}
	}
	_, err := downloadAttachments(context.Background(), atts, t.TempDir())
	if err == nil {
		t.Fatal("expected too many error")
	}
}

func TestDownloadAttachmentsOversize(t *testing.T) {
	_, err := downloadAttachments(context.Background(), []*discordgo.MessageAttachment{
		{Filename: "big.bin", Size: maxAttachmentBytes + 1, URL: "http://example"},
	}, t.TempDir())
	if err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestPromptWithReferenced(t *testing.T) {
	ref := &discordgo.Message{
		ID:      "99",
		Content: "see this crash",
		Author:  &discordgo.User{Username: "alice", GlobalName: "Alice"},
		Attachments: []*discordgo.MessageAttachment{
			{ID: "a1", Filename: "shot.png"},
		},
	}
	got := promptWithReferenced("what is wrong?", ref)
	if !strings.Contains(got, "what is wrong?") {
		t.Fatalf("missing user prompt: %q", got)
	}
	if !strings.Contains(got, "Alice") {
		t.Fatalf("missing author: %q", got)
	}
	if !strings.Contains(got, "see this crash") {
		t.Fatalf("missing ref content: %q", got)
	}
	if !strings.Contains(got, "1 attachment") {
		t.Fatalf("missing attachment note: %q", got)
	}

	got = promptWithReferenced("", ref)
	if !strings.HasPrefix(got, "Please review the referenced Discord message.") {
		t.Fatalf("empty prompt default: %q", got)
	}
	if promptWithReferenced("hi", nil) != "hi" {
		t.Fatal("nil ref should leave prompt unchanged")
	}
}

func TestCollectAttachments(t *testing.T) {
	primary := []*discordgo.MessageAttachment{
		{ID: "1", Filename: "a.txt", URL: "http://x/a"},
		{ID: "2", Filename: "b.txt", URL: "http://x/b"},
	}
	related := &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{ID: "2", Filename: "b.txt", URL: "http://x/b"}, // dupe
			{ID: "3", Filename: "c.png", URL: "http://x/c"},
		},
	}
	got := collectAttachments(primary, related)
	if len(got) != 3 {
		t.Fatalf("got %d want 3", len(got))
	}
	if n := len(collectAttachments(nil, nil)); n != 0 {
		t.Fatalf("empty merge len=%d", n)
	}
}

func TestHasMessageReference(t *testing.T) {
	if hasMessageReference(nil) {
		t.Fatal("nil should be false")
	}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{}}
	if hasMessageReference(m) {
		t.Fatal("no reference should be false")
	}
	m.MessageReference = &discordgo.MessageReference{MessageID: "1"}
	if !hasMessageReference(m) {
		t.Fatal("expected true")
	}
}
