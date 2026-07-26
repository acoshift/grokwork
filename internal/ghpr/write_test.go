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
	base := PRDetail{
		Info:      Info{State: "OPEN"},
		Mergeable: "MERGEABLE",
	}
	base.Checks = "✓ 1"

	ok := CheckMergePreflight(base, false)
	if !ok.Allow {
		t.Fatalf("%+v", ok)
	}

	merged := base
	merged.State = "MERGED"
	if CheckMergePreflight(merged, false).Allow {
		t.Fatal("merged should refuse")
	}

	conflict := base
	conflict.Mergeable = "CONFLICTING"
	if CheckMergePreflight(conflict, false).Allow {
		t.Fatal("conflict should refuse")
	}

	fail := base
	fail.Checks = "✓ 1 · ✗ 1"
	if CheckMergePreflight(fail, false).Allow {
		t.Fatal("failing checks should refuse")
	}
	if !CheckMergePreflight(fail, true).Allow {
		t.Fatal("attempt anyway should allow failing checks")
	}

	draft := base
	draft.IsDraft = true
	if pre := CheckMergePreflight(draft, false); pre.Allow || !strings.Contains(pre.Reason, "draft") {
		t.Fatalf("draft: %+v", pre)
	}

	blocked := base
	blocked.MergeStateStatus = "BLOCKED"
	// mergeable can still be MERGEABLE under branch protection
	if pre := CheckMergePreflight(blocked, false); pre.Allow || !strings.Contains(pre.Reason, "blocked") {
		t.Fatalf("blocked: %+v", pre)
	}
	// attemptAnyway must not bypass protection
	if CheckMergePreflight(blocked, true).Allow {
		t.Fatal("attempt anyway must not allow BLOCKED")
	}
	blocked.ReviewDecision = "REVIEW_REQUIRED"
	if pre := CheckMergePreflight(blocked, false); pre.Allow || !strings.Contains(pre.Reason, "approving review") {
		t.Fatalf("blocked+review: %+v", pre)
	}

	dirty := base
	dirty.MergeStateStatus = "DIRTY"
	if pre := CheckMergePreflight(dirty, false); pre.Allow || !strings.Contains(pre.Reason, "conflict") {
		t.Fatalf("dirty: %+v", pre)
	}

	behind := base
	behind.MergeStateStatus = "BEHIND"
	if pre := CheckMergePreflight(behind, false); pre.Allow || !strings.Contains(pre.Reason, "behind") {
		t.Fatalf("behind: %+v", pre)
	}

	hooks := base
	hooks.MergeStateStatus = "HAS_HOOKS"
	if CheckMergePreflight(hooks, false).Allow {
		t.Fatal("HAS_HOOKS should refuse")
	}

	unknown := base
	unknown.MergeStateStatus = "UNKNOWN"
	if pre := CheckMergePreflight(unknown, false); pre.Allow || !strings.Contains(pre.Reason, "computing") {
		t.Fatalf("unknown: %+v", pre)
	}

	clean := base
	clean.MergeStateStatus = "CLEAN"
	if !CheckMergePreflight(clean, false).Allow {
		t.Fatal("CLEAN should allow")
	}

	unstable := base
	unstable.MergeStateStatus = "UNSTABLE"
	if !CheckMergePreflight(unstable, false).Allow {
		t.Fatal("UNSTABLE alone should allow (non-required checks)")
	}
}

func TestMergeStateHelpers(t *testing.T) {
	if !MergeStateBlocksMerge("BLOCKED") || !MergeStateBlocksMerge("UNKNOWN") {
		t.Fatal("expected blocks")
	}
	if MergeStateBlocksMerge("") || MergeStateBlocksMerge("CLEAN") {
		t.Fatal("empty/CLEAN must not block")
	}
	if !MergeStateAllowsShip("") || !MergeStateAllowsShip("CLEAN") || !MergeStateAllowsShip("UNSTABLE") {
		t.Fatal("expected allows ship")
	}
	if MergeStateAllowsShip("UNKNOWN") || MergeStateAllowsShip("BLOCKED") {
		t.Fatal("UNKNOWN/BLOCKED must not be ship-ready")
	}
	if got := MergeStateBlockReason("BLOCKED", "REVIEW_REQUIRED"); !strings.Contains(got, "approving review") {
		t.Fatalf("reason=%q", got)
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

// --- gh pr review (real GitHub review, satisfies branch protection) ---

// TestSubmitReviewVerdictFlags pins each verdict to its gh flag. A verdict that
// silently mapped to the wrong flag would file an approval where changes were
// requested, under the bot's GitHub identity.
func TestSubmitReviewVerdictFlags(t *testing.T) {
	cases := []struct {
		in       string
		wantFlag string
		notFlags []string
	}{
		{"approve", "--approve", []string{"--request-changes", "--comment"}},
		{"approved", "--approve", []string{"--request-changes", "--comment"}},
		{"request-changes", "--request-changes", []string{"--approve", "--comment"}},
		{"changes_requested", "--request-changes", []string{"--approve", "--comment"}},
		{"comment", "--comment", []string{"--approve", "--request-changes"}},
		{"commented", "--comment", []string{"--approve", "--request-changes"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var saw []string
			run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
				saw = append([]string{name}, args...)
				return []byte("ok"), nil
			}
			v := NormalizeReviewVerdict(tc.in)
			if v == "" {
				t.Fatalf("verdict %q did not normalize", tc.in)
			}
			if err := SubmitReviewWith(context.Background(), run, "/repo", "acme", "app", 9, v, "note"); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(saw, " ")
			if !strings.Contains(joined, "gh pr review 9") {
				t.Fatalf("want gh pr review 9: %v", saw)
			}
			if !strings.Contains(joined, tc.wantFlag) {
				t.Fatalf("want %s: %v", tc.wantFlag, saw)
			}
			for _, no := range tc.notFlags {
				if strings.Contains(joined, no) {
					t.Fatalf("must not pass %s: %v", no, saw)
				}
			}
			if !strings.Contains(joined, "--repo acme/app") {
				t.Fatalf("want repo: %v", saw)
			}
			for _, a := range saw {
				if strings.Contains(a, "--admin") || strings.Contains(a, "bypass") {
					t.Fatalf("must never pass protection-bypass args: %v", saw)
				}
			}
		})
	}
}

// TestSubmitReviewBodyViaTempFile: the body is prose that routinely carries
// backticks and newlines, so it must reach gh through --body-file, never argv.
func TestSubmitReviewBodyViaTempFile(t *testing.T) {
	const body = "Looks good.\n\nBut see `cmd/#main` — & retry?"
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
				if string(b) != body {
					t.Fatalf("body file=%q", b)
				}
			}
		}
		return []byte("ok"), nil
	}
	if err := SubmitReviewWith(context.Background(), run, "/repo", "o", "r", 3, ReviewApprove, body); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "--body-file") {
		t.Fatalf("want --body-file: %v", saw)
	}
	if strings.Contains(joined, "--body ") || strings.Contains(joined, body) {
		t.Fatalf("body must not reach argv: %v", saw)
	}
	if bodyPath == "" {
		t.Fatal("no body file")
	}
	if _, err := os.Stat(bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file should be removed: %v", err)
	}
}

func TestSubmitReviewApproveWithoutBodyOmitsBodyFile(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return []byte("ok"), nil
	}
	if err := SubmitReviewWith(context.Background(), run, "/repo", "o", "r", 3, ReviewApprove, "   "); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(saw, " "), "--body-file") {
		t.Fatalf("bare approve needs no body file: %v", saw)
	}
}

// gh rejects --comment / --request-changes without a body; refuse before exec so
// the caller learns which field is missing instead of reading an argument error.
func TestSubmitReviewRequiresBodyForNonApprove(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}
	for _, v := range []ReviewVerdict{ReviewCommentOnly, ReviewRequestChanges} {
		if err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 1, v, "  "); err == nil {
			t.Fatalf("%s with empty body must fail", v)
		}
	}
}

// An unrecognized verdict is never defaulted: guessing files the wrong review.
func TestSubmitReviewRejectsUnknownVerdict(t *testing.T) {
	if v := NormalizeReviewVerdict("lgtm-ish"); v != "" {
		t.Fatalf("unknown verdict normalized to %q", v)
	}
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}
	if err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 1, "lgtm-ish", "b"); err == nil {
		t.Fatal("expected error for unknown verdict")
	}
	if err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 1, "", "b"); err == nil {
		t.Fatal("expected error for empty verdict")
	}
	if err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 0, ReviewApprove, "b"); err == nil {
		t.Fatal("expected invalid PR number")
	}
}

// A refused review (self-approval, not a collaborator) must surface gh's reason,
// not be swallowed into a silent success.
func TestSubmitReviewSurfacesGHFailure(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh pr review 9 --approve: can not approve your own pull request")
	}
	err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 9, ReviewApprove, "")
	if err == nil {
		t.Fatal("expected gh failure to surface")
	}
	if !strings.Contains(err.Error(), "can not approve your own pull request") {
		t.Fatalf("lost gh reason: %v", err)
	}
}

// gh can be verbose; the reason is kept but bounded so one refusal cannot fill a
// flash message.
func TestSubmitReviewTruncatesLongFailure(t *testing.T) {
	long := strings.Repeat("x", 4000)
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh pr review: %s", long)
	}
	err := SubmitReviewWith(context.Background(), run, "/r", "o", "r", 9, ReviewApprove, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 600 {
		t.Fatalf("error not truncated: %d bytes", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "…") {
		t.Fatalf("want truncation marker: %q", err.Error()[max(0, len(err.Error())-20):])
	}
}
