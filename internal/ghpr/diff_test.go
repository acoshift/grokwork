package ghpr

import (
	"context"
	"fmt"
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
				"mergeStateStatus":"CLEAN",
				"author":{"login":"zoe"},
				"additions":10,
				"deletions":2,
				"changedFiles":3,
				"comments":[]
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
	if d.Number != 9 || d.Body != "hello body" || d.Mergeable != "MERGEABLE" || d.MergeStateStatus != "CLEAN" {
		t.Fatalf("%+v", d)
	}
	// Ensure we asked gh for mergeStateStatus.
	// (run already succeeded; field parse covers the response.)
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

// TestViewPRDetailComments pins the comment cap: a long-lived PR must not drop
// hundreds of cards onto the detail page, the NEWEST ones are the ones kept,
// and the drop is reported rather than silent.
func TestViewPRDetailComments(t *testing.T) {
	makeRun := func(n int) Runner {
		var b strings.Builder
		b.WriteString(`{"number":9,"url":"https://github.com/o/r/pull/9","state":"OPEN","body":"b","comments":[`)
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"author":{"login":"u%d"},"body":"c%d","url":"u","createdAt":"2026-07-2%dT10:00:00Z"}`,
				i, i, i%10)
		}
		b.WriteString(`]}`)
		payload := b.String()
		return func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if strings.HasPrefix(strings.Join(args, " "), "pr view") {
				return []byte(payload), nil
			}
			return []byte(`[]`), nil
		}
	}

	d, err := ViewPRDetailWith(context.Background(), makeRun(3), "/repo", "9")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != 3 || d.CommentsOmitted != 0 {
		t.Fatalf("under the cap: n=%d omitted=%d", len(d.Comments), d.CommentsOmitted)
	}
	if d.Comments[0].Author != "u0" || d.Comments[0].Body != "c0" {
		t.Fatalf("comment 0 = %+v", d.Comments[0])
	}
	if d.Comments[0].CreatedAt.IsZero() {
		t.Fatal("createdAt not parsed")
	}
	// A long comment must not make the page claim the *description* was cut.
	if d.Truncated {
		t.Fatal("comments must not set the body-truncation flag")
	}

	over := MaxPRComments + 7
	d, err = ViewPRDetailWith(context.Background(), makeRun(over), "/repo", "9")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != MaxPRComments || d.CommentsOmitted != 7 {
		t.Fatalf("over the cap: n=%d omitted=%d", len(d.Comments), d.CommentsOmitted)
	}
	// Newest kept, oldest dropped.
	if last := d.Comments[len(d.Comments)-1]; last.Body != fmt.Sprintf("c%d", over-1) {
		t.Fatalf("cap dropped the wrong end: last=%q", last.Body)
	}
}

// TestViewPRDetailCommentsAreBounded pins the production path: gh pr view is
// not asked for comments, because that flag paginates the entire thread, and
// the newest MaxPRComments come back from one comments(last:) query.
func TestViewPRDetailCommentsAreBounded(t *testing.T) {
	const total = MaxPRComments + 7
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			for i, a := range args {
				if a == "--json" && i+1 < len(args) && strings.Contains(args[i+1], "comments") {
					t.Errorf("pr view --json must not request comments: %s", args[i+1])
				}
			}
			return []byte(`{
				"number":9,
				"url":"https://github.com/o/r/pull/9",
				"state":"OPEN",
				"body":"b"
			}`), nil
		case strings.Contains(joined, "api graphql"):
			if !strings.Contains(joined, "comments(last:") {
				t.Errorf("comment query must be bounded: %s", joined)
			}
			if !strings.Contains(joined, "last="+fmt.Sprint(MaxPRComments)) {
				t.Errorf("comment query last=%s", joined)
			}
			var b strings.Builder
			b.WriteString(`{"data":{"repository":{"pullRequest":{"comments":{"totalCount":`)
			fmt.Fprintf(&b, "%d", total)
			b.WriteString(`,"nodes":[`)
			for i := range MaxPRComments {
				if i > 0 {
					b.WriteByte(',')
				}
				// Window is the newest MaxPRComments, oldest first inside it.
				n := total - MaxPRComments + i
				fmt.Fprintf(&b, `{"author":{"login":"u%d"},"body":"c%d","url":"u","createdAt":"2026-07-21T10:00:00Z"}`, n, n)
			}
			b.WriteString(`]}}}}}`)
			return []byte(b.String()), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		default:
			t.Fatalf("unexpected %v", args)
			return nil, nil
		}
	}
	d, err := ViewPRDetailWith(t.Context(), run, "/repo", "https://github.com/o/r/pull/9")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != MaxPRComments || d.CommentsOmitted != 7 {
		t.Fatalf("bounded comments: n=%d omitted=%d", len(d.Comments), d.CommentsOmitted)
	}
	if d.Comments[0].Body != fmt.Sprintf("c%d", total-MaxPRComments) {
		t.Fatalf("window started at %q", d.Comments[0].Body)
	}
	if last := d.Comments[len(d.Comments)-1]; last.Body != fmt.Sprintf("c%d", total-1) || last.Author != fmt.Sprintf("u%d", total-1) {
		t.Fatalf("newest comment = %+v", last)
	}
	if d.Checks != "✓ 1" {
		t.Fatalf("checks=%q", d.Checks)
	}
	if d.Truncated {
		t.Fatal("comments must not set the body-truncation flag")
	}
}

func TestViewPRDetailCommentFailureKeepsSnapshot(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{
				"number":9,
				"url":"https://github.com/o/r/pull/9",
				"title":"T",
				"state":"OPEN",
				"body":"b",
				"mergeable":"MERGEABLE"
			}`), nil
		case strings.Contains(joined, "api graphql"):
			return nil, fmt.Errorf("boom")
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		default:
			t.Errorf("unexpected %v", args)
			return nil, fmt.Errorf("unexpected")
		}
	}
	d, err := ViewPRDetailWith(t.Context(), run, "/repo", "https://github.com/o/r/pull/9")
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "T" || d.Mergeable != "MERGEABLE" || d.Checks != "✓ 1" {
		t.Fatalf("snapshot lost: %+v", d)
	}
	if len(d.Comments) != 0 || d.CommentsOmitted != 0 {
		t.Fatalf("comments=%d omitted=%d", len(d.Comments), d.CommentsOmitted)
	}
}

func TestViewPRDetailOptsSkipsComments(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{"number":9,"url":"https://github.com/o/r/pull/9","state":"OPEN","body":"b"}`), nil
		case strings.Contains(joined, "api graphql"):
			t.Errorf("gates path fetched comments: %s", joined)
			return []byte(`{"data":{"repository":{"pullRequest":{"comments":{"totalCount":1,"nodes":[{"author":{"login":"a"},"body":"nope","url":"u","createdAt":"2026-07-21T10:00:00Z"}]}}}}}`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	d, err := ViewPRDetailOpts(t.Context(), run, "/repo", "https://github.com/o/r/pull/9", PRDetailOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != 0 || d.CommentsOmitted != 0 {
		t.Fatalf("comments=%d omitted=%d", len(d.Comments), d.CommentsOmitted)
	}
}
