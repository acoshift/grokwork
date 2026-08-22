package bot

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Minimal PNG (signature only) — http.DetectContentType returns image/png.
var tinyPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}

func webUpload(name string, data []byte) WebUpload {
	return WebUpload{
		Filename: name,
		Size:     int64(len(data)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

func TestSaveWebAttachmentsEmpty(t *testing.T) {
	b, _ := testBotWithData(t)
	paths, cleanup, err := b.SaveWebAttachments(nil)
	if err != nil || paths != nil {
		t.Fatalf("empty: paths=%v err=%v", paths, err)
	}
	cleanup()
}

func TestSaveWebAttachmentsHappy(t *testing.T) {
	b, _ := testBotWithData(t)
	paths, cleanup, err := b.SaveWebAttachments([]WebUpload{
		webUpload("shot.png", tinyPNG),
		webUpload("shot.png", tinyPNG), // uniquify
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if len(paths) != 2 {
		t.Fatalf("paths=%d", len(paths))
	}
	root := webStagingRoot(b.cfg.DataDir)
	for _, p := range paths {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Fatalf("path %q not under staging root %q", p, root)
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(raw, tinyPNG) {
			t.Fatalf("content mismatch for %s", p)
		}
	}
	if filepath.Base(paths[0]) != "shot.png" {
		t.Fatalf("first name=%q", filepath.Base(paths[0]))
	}
	if filepath.Base(paths[1]) != "shot_2.png" {
		t.Fatalf("second name=%q want shot_2.png", filepath.Base(paths[1]))
	}
}

func TestSaveWebAttachmentsPDFAndText(t *testing.T) {
	b, _ := testBotWithData(t)
	pdf := []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	txt := []byte("hello world")
	paths, cleanup, err := b.SaveWebAttachments([]WebUpload{
		webUpload("spec.pdf", pdf),
		webUpload("notes.txt", txt),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if len(paths) != 2 {
		t.Fatalf("paths=%d", len(paths))
	}
	gotPDF, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPDF, pdf) {
		t.Fatalf("pdf content mismatch")
	}
	gotTxt, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotTxt, txt) {
		t.Fatalf("txt content mismatch")
	}
}

func TestSaveWebAttachmentsPerFileLimit(t *testing.T) {
	b, _ := testBotWithData(t)
	// Declared size alone is enough to refuse.
	_, _, err := b.SaveWebAttachments([]WebUpload{{
		Filename: "big.png",
		Size:     maxAttachmentBytes + 1,
		Open: func() (io.ReadCloser, error) {
			t.Fatal("Open must not be called when declared size is over limit")
			return nil, nil
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "max per file") {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveWebAttachmentsTotalLimit(t *testing.T) {
	b, _ := testBotWithData(t)
	// Three under-per-file sizes that sum past maxTotalBytes.
	chunk := int64(maxTotalBytes/3 + 1) // ~16.7 MiB each → ~50.1 MiB total
	if chunk > maxAttachmentBytes {
		t.Fatalf("test setup: chunk %d exceeds per-file max", chunk)
	}
	_, _, err := b.SaveWebAttachments([]WebUpload{
		{Filename: "a.png", Size: chunk, Open: func() (io.ReadCloser, error) { return nil, nil }},
		{Filename: "b.png", Size: chunk, Open: func() (io.ReadCloser, error) { return nil, nil }},
		{Filename: "c.png", Size: chunk, Open: func() (io.ReadCloser, error) { return nil, nil }},
	})
	if err == nil || !strings.Contains(err.Error(), "max is") {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveWebAttachmentsTooMany(t *testing.T) {
	b, _ := testBotWithData(t)
	ups := make([]WebUpload, maxAttachments+1)
	for i := range ups {
		ups[i] = webUpload("x.png", tinyPNG)
	}
	_, _, err := b.SaveWebAttachments(ups)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveWebAttachmentsCleanup(t *testing.T) {
	b, _ := testBotWithData(t)
	paths, cleanup, err := b.SaveWebAttachments([]WebUpload{webUpload("a.png", tinyPNG)})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths=%v", paths)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("cleanup left file: %v", err)
	}
	dir := filepath.Dir(paths[0])
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cleanup left dir: %v", err)
	}
}

func TestRemoveWebStagedAttachments(t *testing.T) {
	dir := t.TempDir()
	root := webStagingRoot(dir)
	stage := filepath.Join(root, "abc")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(stage, "x.png")
	if err := os.WriteFile(p, tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "other.png")
	if err := os.WriteFile(outside, tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	removeWebStagedAttachments(dir, []string{p, outside})
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("staged file should be gone")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside path must not be touched")
	}
}
