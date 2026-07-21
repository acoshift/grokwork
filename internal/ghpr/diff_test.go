package ghpr

import (
	"context"
	"strings"
	"testing"
)

const samplePatch = `diff --git a/foo.go b/foo.go
index 111..222 100644
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,4 @@
 package foo
+import "fmt"
 
 func X() {}
diff --git a/bar.go b/bar.go
--- a/bar.go
+++ b/bar.go
@@ -1 +1 @@
-old
+new
`

func TestParseUnifiedDiff(t *testing.T) {
	d := ParseUnifiedDiff([]byte(samplePatch), DiffCaps{})
	if len(d.Files) != 2 {
		t.Fatalf("files=%d %+v", len(d.Files), d.Files)
	}
	if d.Files[0].PathNew != "foo.go" || d.Files[1].PathNew != "bar.go" {
		t.Fatalf("paths=%v", d.FilePaths())
	}
	if d.HunkCount() != 2 {
		t.Fatalf("hunks=%d", d.HunkCount())
	}
	if !strings.HasPrefix(d.Files[0].Hunks[0].Header, "@@") {
		t.Fatalf("header=%q", d.Files[0].Hunks[0].Header)
	}
	foundPlus := false
	for _, line := range d.Files[0].Hunks[0].Lines {
		if strings.HasPrefix(line, "+import") {
			foundPlus = true
		}
	}
	if !foundPlus {
		t.Fatalf("missing + line: %+v", d.Files[0].Hunks[0].Lines)
	}
	if d.Truncated {
		t.Fatal("should not truncate full sample")
	}
}

func TestParseUnifiedDiffCaps(t *testing.T) {
	// Max files
	d := ParseUnifiedDiff([]byte(samplePatch), DiffCaps{MaxFiles: 1, MaxPatchBytes: 1 << 20, MaxHunks: 100})
	if len(d.Files) != 1 || !d.Truncated {
		t.Fatalf("max files: files=%d trunc=%v", len(d.Files), d.Truncated)
	}
	// Max patch bytes
	d = ParseUnifiedDiff([]byte(samplePatch), DiffCaps{MaxPatchBytes: 40, MaxFiles: 50, MaxHunks: 100})
	if !d.Truncated {
		t.Fatal("expected trunc on max bytes")
	}
	// Max hunks
	d = ParseUnifiedDiff([]byte(samplePatch), DiffCaps{MaxHunks: 1, MaxFiles: 50, MaxPatchBytes: 1 << 20})
	if d.HunkCount() != 1 || !d.Truncated {
		t.Fatalf("hunks=%d trunc=%v", d.HunkCount(), d.Truncated)
	}
	// Empty / nil safe
	d = ParseUnifiedDiff(nil, DiffCaps{})
	if len(d.Files) != 0 {
		t.Fatal("nil patch")
	}
}

func TestPRDiffWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "pr diff 7") {
			t.Fatalf("args=%v", args)
		}
		return []byte(samplePatch), nil
	}
	d, err := PRDiffWith(context.Background(), run, "/repo", "7", DiffOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[0].PathNew != "foo.go" {
		t.Fatalf("%+v", d)
	}
}

func TestPRDiffNameOnly(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--name-only") {
			t.Fatalf("args=%v", args)
		}
		return []byte("a.go\nb.go\n"), nil
	}
	d, err := PRDiffWith(context.Background(), run, "/r", "1", DiffOpts{NameOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[0].PathNew != "a.go" {
		t.Fatalf("%+v", d)
	}
}

func TestWorktreeDiffWithMock(t *testing.T) {
	var calls []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("%s %v", name, args)
		}
		if dir != "/wt" {
			t.Fatalf("dir=%q", dir)
		}
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		switch {
		case args[0] == "merge-base":
			if joined != "merge-base origin/main HEAD" {
				t.Fatalf("merge-base args = %q", joined)
			}
			return []byte("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n"), nil
		case args[0] == "diff":
			// Must diff against merge-base, not tip of origin/main.
			if args[1] != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
				t.Fatalf("diff left = %q, want merge-base sha", args[1])
			}
			return []byte(samplePatch), nil
		default:
			t.Fatalf("unexpected git %v", args)
			return nil, nil
		}
	}
	d, err := WorktreeDiffWith(context.Background(), run, "/wt", "origin/main", DiffCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("%+v", d)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestWorktreeDiffHEADSkipsMergeBase(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != "git" || args[0] != "diff" || args[1] != "HEAD" {
			t.Fatalf("want git diff HEAD, got %s %v", name, args)
		}
		return []byte(samplePatch), nil
	}
	if _, err := WorktreeDiffWith(context.Background(), run, "/wt", "HEAD", DiffCaps{}); err != nil {
		t.Fatal(err)
	}
	if _, err := WorktreeDiffWith(context.Background(), run, "/wt", "", DiffCaps{}); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeDiffRejectsEmptyCwd(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("runner must not be called with empty cwd")
		return nil, nil
	}
	_, err := WorktreeDiffWith(context.Background(), run, "", "origin/main", DiffCaps{})
	if err == nil {
		t.Fatal("expected error for empty cwd")
	}
}

func TestViewPRDetailWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{
				"number":9,
				"url":"https://github.com/o/r/pull/9",
				"title":"T",
				"state":"OPEN",
				"isDraft":false,
				"reviewDecision":"APPROVED",
				"headRefOid":"abc",
				"headRefName":"feat",
				"baseRefName":"main",
				"body":"hello body",
				"mergeable":"MERGEABLE",
				"author":{"login":"zoe"},
				"additions":10,
				"deletions":2,
				"changedFiles":3
			}`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		default:
			t.Fatalf("unexpected %v", args)
			return nil, nil
		}
	}
	d, err := ViewPRDetailWith(context.Background(), run, "/repo", "9")
	if err != nil {
		t.Fatal(err)
	}
	if d.Number != 9 || d.Body != "hello body" || d.Mergeable != "MERGEABLE" {
		t.Fatalf("%+v", d)
	}
	if d.Author != "zoe" || d.BaseRef != "main" || d.ChangedFiles != 3 {
		t.Fatalf("%+v", d)
	}
	if d.Owner != "o" || d.Repo != "r" {
		t.Fatalf("owner/repo %s/%s", d.Owner, d.Repo)
	}
	if d.Checks != "✓ 1" {
		t.Fatalf("checks=%q", d.Checks)
	}
}
