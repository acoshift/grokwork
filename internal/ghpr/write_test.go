package ghpr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCreateIssueWithURL(t *testing.T) {
	var saw []string
	var bodyPath string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		for _, a := range args {
			if a == "--json" {
				t.Fatal("gh issue create does not support --json")
			}
		}
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
		// Real gh prints the issue URL (no --json on create).
		return []byte("https://github.com/o/r/issues/42\n"), nil
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
	if strings.Contains(joined, "--json") {
		t.Fatalf("must not pass --json: %v", saw)
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
		for _, a := range args {
			if a == "--json" {
				t.Fatal("gh issue create does not support --json")
			}
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--label") {
			return nil, fmt.Errorf("label missing")
		}
		return []byte("https://github.com/o/r/issues/7\n"), nil
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

func TestParseCreateIssueOutputJSON(t *testing.T) {
	// Defensive: still accept JSON if a wrapper or future gh ever emits it.
	n, url, err := parseCreateIssueOutput([]byte(`{"number":3,"url":"https://github.com/o/r/issues/3"}`))
	if err != nil || n != 3 || url != "https://github.com/o/r/issues/3" {
		t.Fatalf("n=%d url=%s err=%v", n, url, err)
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

func TestCloseIssueWithComment(t *testing.T) {
	var calls []string
	var bodyPath string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		for i, a := range args {
			if a == "--body-file" && i+1 < len(args) {
				bodyPath = args[i+1]
				b, err := os.ReadFile(bodyPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "done" {
					t.Fatalf("body=%q", b)
				}
			}
		}
		return nil, nil
	}
	if err := CloseIssueWith(context.Background(), run, "/repo", "o", "r", 12, "done"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
	if !strings.Contains(calls[0], "issue comment 12") || !strings.Contains(calls[0], "--body-file") {
		t.Fatalf("comment call=%q", calls[0])
	}
	if !strings.Contains(calls[1], "issue close 12") || !strings.Contains(calls[1], "--repo o/r") {
		t.Fatalf("close call=%q", calls[1])
	}
	if bodyPath == "" {
		t.Fatal("no body file")
	}
	if _, err := os.Stat(bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file should be removed: %v", err)
	}
}

func TestCloseIssueNoComment(t *testing.T) {
	var calls []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := CloseIssueWith(context.Background(), run, "/repo", "o", "r", 5, "  "); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "issue close 5") {
		t.Fatalf("calls=%v", calls)
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

func TestRequestReviewersWithMappedLogin(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return []byte("ok"), nil
	}
	if err := RequestReviewersWith(context.Background(), run, "/repo", "acme", "app", 9, "@alice-gh"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "pr edit 9") {
		t.Fatalf("want pr edit: %v", saw)
	}
	if !strings.Contains(joined, "--add-reviewer alice-gh") {
		t.Fatalf("want stripped login: %v", saw)
	}
	if !strings.Contains(joined, "--repo acme/app") {
		t.Fatalf("want repo: %v", saw)
	}
	// Multi-login comma join
	saw = nil
	if err := RequestReviewersWith(context.Background(), run, "/repo", "o", "r", 1, "a", "@b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(saw, " "), "--add-reviewer a,b") {
		t.Fatalf("%v", saw)
	}
}

func TestRequestReviewersRejectsEmpty(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}
	if err := RequestReviewersWith(context.Background(), run, "/r", "o", "r", 1); err == nil {
		t.Fatal("expected error for no logins")
	}
	if err := RequestReviewersWith(context.Background(), run, "/r", "o", "r", 1, "  ", "@"); err == nil {
		t.Fatal("expected error for blank logins")
	}
	if err := RequestReviewersWith(context.Background(), run, "/r", "o", "r", 0, "x"); err == nil {
		t.Fatal("expected invalid PR number")
	}
}
