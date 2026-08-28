package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestLastNLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 3, "a\nb\nc\n"},
		{"a\nb\nc\n", 10, "a\nb\nc\n"},
		{"hello\n", 1, "hello\n"},
		{"hello", 1, "hello"},
		{"", 5, ""},
		{"only\n", 0, ""},
		{"\n\n\n", 2, "\n\n"},
		{"x", 1, "x"},
	}
	for _, tc := range cases {
		got := string(lastNLines([]byte(tc.in), tc.n))
		if got != tc.want {
			t.Fatalf("n=%d in=%q got=%q want=%q", tc.n, tc.in, got, tc.want)
		}
	}
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb\nc\n", 3},
		{"a\nb\nc", 3},
		{"\n", 1},
	}
	for _, tc := range cases {
		if got := countLines([]byte(tc.in)); got != tc.want {
			t.Fatalf("in=%q got=%d want=%d", tc.in, got, tc.want)
		}
	}
}

func TestTrimLogFileKeepsTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	writeLogLines(t, path, 20, "L")
	res, err := trimLogFile(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("expected rewrite")
	}
	if res.KeptLines != 5 {
		t.Fatalf("kept=%d", res.KeptLines)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := logLines(15, 20, "L")
	if string(got) != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestTrimLogFileSkipsWhenAlreadyShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	body := logLines(0, 3, "S")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := trimLogFile(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("should skip rewrite")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("rewrote a short file: %q", got)
	}
}

func TestTrimLogFileMissingAndDisabled(t *testing.T) {
	dir := t.TempDir()
	res, err := trimLogFile(filepath.Join(dir, "stderr.log"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("missing file should skip")
	}
	res, err = trimLogFile(filepath.Join(dir, "stdout.log"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("keepLines=0 should skip")
	}
}

func TestTrimLogFileAppendWriterContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString(logLines(0, 12, "A")); err != nil {
		t.Fatal(err)
	}

	res, err := trimLogFile(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("expected trim")
	}

	if _, err := w.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := logLines(9, 12, "A") + "after\n"
	if string(got) != want {
		t.Fatalf("got=%q want=%q size=%d", got, want, len(got))
	}
}

func TestTrimLogFileResyncsMatchingStdio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(logLines(0, 20, "R")); err != nil {
		t.Fatal(err)
	}

	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old }()

	res, err := trimLogFile(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("expected trim")
	}
	if _, err := os.Stderr.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := logLines(17, 20, "R") + "new\n"
	if string(got) != want {
		t.Fatalf("got=%q want=%q size=%d", got, want, len(got))
	}
}

func TestTrimLogFileByteCap(t *testing.T) {
	orig := logTailMaxBytes
	logTailMaxBytes = 32
	defer func() { logTailMaxBytes = orig }()

	path := filepath.Join(t.TempDir(), "stderr.log")
	body := strings.Repeat("abcdefghij\n", 20) // 11*20 = 220 bytes
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := trimLogFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("byte cap should still rewrite a large file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) > 32 {
		t.Fatalf("kept %d bytes, cap 32", len(got))
	}
	if !strings.HasSuffix(string(got), "abcdefghij\n") {
		t.Fatalf("tail should be complete lines, got %q", got)
	}
}

func TestRunLogTrimCycle(t *testing.T) {
	dir := t.TempDir()
	writeLogLines(t, filepath.Join(dir, "stderr.log"), 40, "E")
	writeLogLines(t, filepath.Join(dir, "stdout.log"), 4, "O")
	b := &Bot{cfg: &config.Config{DataDir: dir, LogTailLines: new(8)}}
	b.runLogTrimCycle("test")

	stderr, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stderr) != logLines(32, 40, "E") {
		t.Fatalf("stderr=%q", stderr)
	}
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != logLines(0, 4, "O") {
		t.Fatalf("stdout should be untouched: %q", stdout)
	}
}

func TestRunLogTrimCycleDisabled(t *testing.T) {
	dir := t.TempDir()
	body := logLines(0, 12, "Z")
	path := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: &config.Config{DataDir: dir, LogTailLines: new(0)}}
	b.runLogTrimCycle("test")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("disabled trim rewrote file: %q", got)
	}
}

func writeLogLines(t *testing.T, path string, n int, prefix string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(logLines(0, n, prefix)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func logLines(from, to int, prefix string) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}
