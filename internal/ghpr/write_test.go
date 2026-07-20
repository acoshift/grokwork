package ghpr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCreateIssueWithJSON(t *testing.T) {
	var saw []string
	var bodyPath string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		for i, a := range args {
			if a == "--body-file" && i+1 < len(args) {
				bodyPath = args[i+1]
				b, err := os.ReadFile(bodyPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "body text" {
					t.Fatalf("body=%q", b)
				}
			}
		}
		return []byte(`{"number":42,"url":"https://github.com/o/r/issues/42"}`), nil
	}
	n, url, err := CreateIssueWith(context.Background(), run, "/repo", "o", "r", CreateIssueOpts{
		Title:  "Bug",
		Body:   "body text",
		Labels: []string{"commit-review"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 || url != "https://github.com/o/r/issues/42" {
		t.Fatalf("n=%d url=%s", n, url)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "issue create") || !strings.Contains(joined, "--title Bug") {
		t.Fatalf("%v", saw)
	}
	if !strings.Contains(joined, "--label commit-review") || !strings.Contains(joined, "--repo o/r") {
		t.Fatalf("%v", saw)
	}
	if bodyPath == "" {
		t.Fatal("no body file")
	}
	if _, err := os.Stat(bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file should be removed: %v", err)
	}
}

func TestCreateIssueEmptyTitle(t *testing.T) {
	_, _, err := CreateIssueWith(context.Background(), nil, "/r", "o", "r", CreateIssueOpts{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateIssueLabelFallback(t *testing.T) {
	calls := 0
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--label") {
			return nil, fmt.Errorf("label missing")
		}
		return []byte(`{"number":7,"url":"https://github.com/o/r/issues/7"}`), nil
	}
	n, _, err := CreateIssueWith(context.Background(), run, "/repo", "o", "r", CreateIssueOpts{
		Title:  "T",
		Body:   "B",
		Labels: []string{"missing-label"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 || calls != 2 {
		t.Fatalf("n=%d calls=%d", n, calls)
	}
}

func TestParseCreateIssueOutputURL(t *testing.T) {
	n, url, err := parseCreateIssueOutput([]byte("https://github.com/acme/app/issues/99\n"))
	if err != nil || n != 99 || !strings.Contains(url, "/issues/99") {
		t.Fatalf("n=%d url=%s err=%v", n, url, err)
	}
}

func TestCommentIssueUsesBodyFile(t *testing.T) {
	var saw []string
	var bodyPath string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		for i, a := range args {
			if a == "--body-file" && i+1 < len(args) {
				bodyPath = args[i+1]
				b, err := os.ReadFile(bodyPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "hello #1" {
					t.Fatalf("body file=%q", b)
				}
			}
		}
		return []byte("ok"), nil
	}
	if err := CommentIssueWith(context.Background(), run, "/repo", "o", "r", 3, "hello #1"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "issue comment 3") || !strings.Contains(joined, "--body-file") {
		t.Fatalf("args=%v", saw)
	}
	if !strings.Contains(joined, "--repo o/r") {
		t.Fatalf("missing --repo: %v", saw)
	}
	if bodyPath == "" {
		t.Fatal("no body file")
	}
	// temp file cleaned up
	if _, err := os.Stat(bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file should be removed: %v", err)
	}
}

func TestCommentPRAndClose(t *testing.T) {
	var last []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		last = append([]string{name}, args...)
		return nil, nil
	}
	if err := CommentPRWith(context.Background(), run, "/r", "a", "b", 9, "note"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(last, " "), "pr comment 9") {
		t.Fatalf("%v", last)
	}
	if err := ClosePRWith(context.Background(), run, "/r", "a", "b", 9); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(last, " "), "pr close 9") || !strings.Contains(strings.Join(last, " "), "--repo a/b") {
		t.Fatalf("%v", last)
	}
}

func TestCheckMergePreflight(t *testing.T) {
	ok := CheckMergePreflight("OPEN", "MERGEABLE", "✓ 1", false)
	if !ok.Allow {
		t.Fatalf("%+v", ok)
	}
	if CheckMergePreflight("MERGED", "MERGEABLE", "✓ 1", false).Allow {
		t.Fatal("merged should refuse")
	}
	if CheckMergePreflight("OPEN", "CONFLICTING", "✓ 1", false).Allow {
		t.Fatal("conflict should refuse")
	}
	fail := CheckMergePreflight("OPEN", "MERGEABLE", "✓ 1 · ✗ 1", false)
	if fail.Allow {
		t.Fatal("failing checks should refuse")
	}
	anyway := CheckMergePreflight("OPEN", "MERGEABLE", "✓ 1 · ✗ 1", true)
	if !anyway.Allow {
		t.Fatal("attempt anyway should allow")
	}
}

func TestMergePRDefaultSquashNoAdmin(t *testing.T) {
	var last []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		last = append([]string{name}, args...)
		for _, a := range args {
			if a == "--admin" {
				t.Fatal("must not pass --admin")
			}
		}
		return nil, nil
	}
	if err := MergePRWith(context.Background(), run, "/r", "o", "r", 4, MergeOpts{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(last, " ")
	if !strings.Contains(joined, "pr merge 4") || !strings.Contains(joined, "--squash") {
		t.Fatalf("%v", last)
	}
	if !strings.Contains(joined, "--repo o/r") {
		t.Fatalf("%v", last)
	}
	if err := MergePRWith(context.Background(), run, "/r", "o", "r", 4, MergeOpts{Method: MergeMerge}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(last, " "), "--merge") {
		t.Fatalf("%v", last)
	}
}

func TestNormalizeMergeMethod(t *testing.T) {
	if NormalizeMergeMethod("") != MergeSquash {
		t.Fatal("default")
	}
	if NormalizeMergeMethod("MERGE") != MergeMerge {
		t.Fatal("merge")
	}
}

func TestEmptyCommentRejected(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}
	if err := CommentIssueWith(context.Background(), run, "/r", "o", "r", 1, "  "); err == nil {
		t.Fatal("expected error")
	}
}
