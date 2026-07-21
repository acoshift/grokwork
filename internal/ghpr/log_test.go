package ghpr

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseCommitLog(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"abc123def456\x1fFix bug\x1fAda\x1fada@ex.com\x1f2026-07-20T12:00:00Z",
		"deadbeef0001\x1fAdd feature\x1fBob\x1fbob@ex.com\x1f2026-07-19T08:30:00+00:00",
	}, "\n"))
	list, err := parseCommitLog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].SHA != "abc123def456" || list[0].ShortSHA != "abc123d" || list[0].Subject != "Fix bug" {
		t.Fatalf("%+v", list[0])
	}
	if list[0].AuthorName != "Ada" || list[0].AuthorEmail != "ada@ex.com" {
		t.Fatalf("author %+v", list[0])
	}
	if !list[0].AuthorDate.Equal(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("date %v", list[0].AuthorDate)
	}
	if list[1].Subject != "Add feature" {
		t.Fatalf("%+v", list[1])
	}
}

func TestParseCommitLogEmpty(t *testing.T) {
	list, err := parseCommitLog(nil)
	if err != nil || list != nil {
		t.Fatalf("%v %v", list, err)
	}
}

func TestListCommitsWith(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		if name != "git" || args[0] != "log" {
			t.Fatalf("unexpected %s %v", name, args)
		}
		return []byte("aa11bb22cc33\x1fHello\x1fX\x1fx@y.z\x1f2026-01-01T00:00:00Z\n"), nil
	}
	list, err := ListCommitsWith(context.Background(), run, "/repo", CommitListOpts{Ref: "main", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Subject != "Hello" {
		t.Fatalf("%+v", list)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "log") || !strings.Contains(joined, "main") || !strings.Contains(joined, "-n 10") {
		t.Fatalf("args %v", saw)
	}
}

func TestFetchWith(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if dir != "/repo" {
			t.Fatalf("dir=%q", dir)
		}
		saw = append([]string{name}, args...)
		return []byte("ok\n"), nil
	}
	if err := FetchWith(context.Background(), run, "/repo"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(saw, " ") != "git fetch --all --prune" {
		t.Fatalf("args %v", saw)
	}
	if err := FetchWith(context.Background(), run, "  "); err == nil {
		t.Fatal("expected empty path error")
	}
}

func TestListCommitsDefaultRefAndCap(t *testing.T) {
	var nArg string
	var ref string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "-n" && i+1 < len(args) {
				nArg = args[i+1]
			}
		}
		if len(args) > 0 {
			ref = args[len(args)-1]
		}
		return nil, nil
	}
	_, err := ListCommitsWith(context.Background(), run, "/r", CommitListOpts{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if nArg != "100" {
		t.Fatalf("limit cap want 100 got %s", nArg)
	}
	if ref != "HEAD" {
		t.Fatalf("ref=%q", ref)
	}
}

func TestShowCommitWith(t *testing.T) {
	full := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hasArg := func(args []string, want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "rev-parse":
			return []byte(full + "\n"), nil
		case hasArg(args, "-s"):
			return []byte(full + "\x1fSubject line\x1fAnn\x1fa@b.c\x1f2026-07-01T10:00:00Z\x1fBody paragraph.\n"), nil
		case hasArg(args, "--stat"):
			return []byte(" foo.go | 2 +-\n 1 file changed\n"), nil
		case hasArg(args, "-p"):
			return []byte(samplePatch), nil
		default:
			t.Fatalf("unexpected args %v", args)
			return nil, nil
		}
	}
	d, err := ShowCommitWith(context.Background(), run, "/repo", "aaaaaaa", DiffCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if d.SHA != full || d.ShortSHA != "aaaaaaa" {
		t.Fatalf("sha %+v", d.CommitSummary)
	}
	if d.Subject != "Subject line" || d.Body != "Body paragraph." {
		t.Fatalf("meta %+v", d)
	}
	if !strings.Contains(d.Stat, "foo.go") {
		t.Fatalf("stat %q", d.Stat)
	}
	if len(d.Diff.Files) != 2 {
		t.Fatalf("diff files=%d", len(d.Diff.Files))
	}
}

func TestShowCommitEmptySHA(t *testing.T) {
	_, err := ShowCommitWith(context.Background(), nil, "/r", "  ", DiffCaps{})
	if err == nil {
		t.Fatal("expected error")
	}
}
