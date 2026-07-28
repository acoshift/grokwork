package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MergeMethod is a gh pr merge strategy.
type MergeMethod string

const (
	MergeSquash MergeMethod = "squash"
	MergeMerge  MergeMethod = "merge"
	MergeRebase MergeMethod = "rebase"
)

// NormalizeMergeMethod returns squash/merge/rebase (default squash).
func NormalizeMergeMethod(m string) MergeMethod {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "merge":
		return MergeMerge
	case "rebase":
		return MergeRebase
	default:
		return MergeSquash
	}
}

// ReviewVerdict is a gh pr review action. Unlike a grokwork team review
// (internal/reviewstore), a review submitted through here is a *real* GitHub
// review by the authenticated gh user, so an approval can satisfy branch
// protection. The values are spelled exactly as gh's flag names so the flag is
// derived from the verdict rather than mapped in a second switch that could
// drift (and turn a request-changes into an approve).
type ReviewVerdict string

const (
	ReviewApprove        ReviewVerdict = "approve"
	ReviewRequestChanges ReviewVerdict = "request-changes"
	// ReviewCommentOnly is named around the ReviewComment struct above; its
	// value is still gh's "comment" flag name.
	ReviewCommentOnly ReviewVerdict = "comment"
)

// NormalizeReviewVerdict maps loose input (a form value, a reviewstore verdict)
// onto a gh review action. Unrecognized input returns "" and is never defaulted:
// every default here is a wrong review submitted under the bot's identity.
func NormalizeReviewVerdict(v string) ReviewVerdict {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), "_", "-")) {
	case "approve", "approved":
		return ReviewApprove
	case "request-changes", "changes-requested", "request-change":
		return ReviewRequestChanges
	case "comment", "commented", "comment-only":
		return ReviewCommentOnly
	}
	return ""
}

// SubmitReview submits a GitHub pull request review as the authenticated gh user.
func SubmitReview(ctx context.Context, repoDir, owner, repo string, number int, verdict ReviewVerdict, body string) error {
	return SubmitReviewWith(ctx, defaultRunner, repoDir, owner, repo, number, verdict, body)
}

// SubmitReviewWith is SubmitReview with an injectable runner.
// Equivalent to: gh pr review N --approve|--request-changes|--comment
// [--body-file …] [--repo owner/repo]
func SubmitReviewWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, verdict ReviewVerdict, body string) error {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return fmt.Errorf("invalid PR number")
	}
	v := NormalizeReviewVerdict(string(verdict))
	if v == "" {
		return fmt.Errorf("invalid review verdict %q", truncateForErr(string(verdict), 40))
	}
	body = strings.TrimSpace(body)
	// gh refuses --request-changes and --comment without a body. Refusing here
	// names the missing field; letting gh do it surfaces an argument error that
	// reads like a bug in the caller.
	if body == "" && v != ReviewApprove {
		return fmt.Errorf("a %q review requires a body", v)
	}
	args := []string{"pr", "review", strconv.Itoa(number), "--" + string(v)}
	if body != "" {
		// Body via temp file, never argv: a review body is arbitrary prose and
		// routinely carries backticks, `#`, and newlines.
		path, cleanup, err := writeBodyFile(body)
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--body-file", path)
	}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	if _, err := run(ctx, repoDir, "gh", args...); err != nil {
		// gh's stderr is the entire diagnostic — "not a collaborator", "can not
		// approve your own pull request", a protected-branch refusal — and it is
		// what the PR page shows, so it is kept rather than replaced. Bounded so
		// one verbose refusal cannot fill a flash message.
		return errors.New(truncateForErr(err.Error(), 500))
	}
	return nil
}

// CreateIssueOpts is input for gh issue create.
type CreateIssueOpts struct {
	Title  string
	Body   string
	Labels []string
}

// CreateIssue creates a GitHub issue and returns its number and URL.
func CreateIssue(ctx context.Context, repoDir, owner, repo string, opts CreateIssueOpts) (number int, url string, err error) {
	return CreateIssueWith(ctx, defaultRunner, repoDir, owner, repo, opts)
}

// CreateIssueWith is CreateIssue with an injectable runner.
func CreateIssueWith(ctx context.Context, run Runner, repoDir, owner, repo string, opts CreateIssueOpts) (number int, url string, err error) {
	if run == nil {
		run = defaultRunner
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		return 0, "", fmt.Errorf("empty issue title")
	}
	path, cleanup, err := writeBodyFile(opts.Body)
	if err != nil {
		return 0, "", err
	}
	defer cleanup()
	// gh issue create does not support --json; it prints the new issue URL on success.
	args := []string{"issue", "create", "--title", title, "--body-file", path}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	for _, lab := range opts.Labels {
		lab = strings.TrimSpace(lab)
		if lab == "" {
			continue
		}
		args = append(args, "--label", lab)
	}
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		// Retry without labels if labels caused failure (missing label in repo).
		if len(opts.Labels) > 0 {
			argsNoLabel := []string{"issue", "create", "--title", title, "--body-file", path}
			if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
				argsNoLabel = append(argsNoLabel, "--repo", o+"/"+r)
			}
			out2, err2 := run(ctx, repoDir, "gh", argsNoLabel...)
			if err2 == nil {
				return parseCreateIssueOutput(out2)
			}
		}
		return 0, "", err
	}
	return parseCreateIssueOutput(out)
}

func parseCreateIssueOutput(out []byte) (number int, url string, err error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return 0, "", fmt.Errorf("gh issue create: empty output")
	}
	// JSON: {"number":1,"url":"https://..."}
	if looksLikeJSON(out) {
		var v struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		}
		if jerr := json.Unmarshal(out, &v); jerr == nil && v.Number > 0 {
			return v.Number, strings.TrimSpace(v.URL), nil
		}
	}
	// Plain URL line: https://github.com/o/r/issues/12
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, "/issues/"); i >= 0 {
		nStr := strings.TrimSpace(line[i+len("/issues/"):])
		if slash := strings.IndexAny(nStr, " \t\n?#"); slash >= 0 {
			nStr = nStr[:slash]
		}
		n, nerr := strconv.Atoi(nStr)
		if nerr == nil && n > 0 {
			return n, line, nil
		}
	}
	return 0, "", fmt.Errorf("gh issue create: could not parse output %q", truncateForErr(string(out), 200))
}

func truncateForErr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CommentIssue posts a comment on a GitHub issue via body-file.
func CommentIssue(ctx context.Context, repoDir, owner, repo string, number int, body string) error {
	return CommentIssueWith(ctx, defaultRunner, repoDir, owner, repo, number, body)
}

// CommentIssueWith is CommentIssue with an injectable runner.
func CommentIssueWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, body string) error {
	if run == nil {
		run = defaultRunner
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("empty comment body")
	}
	if number <= 0 {
		return fmt.Errorf("invalid issue number")
	}
	path, cleanup, err := writeBodyFile(body)
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"issue", "comment", strconv.Itoa(number), "--body-file", path}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	_, err = run(ctx, repoDir, "gh", args...)
	return err
}

// CommentPR posts a comment on a pull request via body-file.
func CommentPR(ctx context.Context, repoDir, owner, repo string, number int, body string) error {
	return CommentPRWith(ctx, defaultRunner, repoDir, owner, repo, number, body)
}

// CommentPRWith is CommentPR with an injectable runner.
func CommentPRWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, body string) error {
	_, err := CommentPRWithURL(ctx, run, repoDir, owner, repo, number, body)
	return err
}

// CommentPRWithURL posts a PR comment and returns the comment URL when gh prints one.
func CommentPRWithURL(ctx context.Context, run Runner, repoDir, owner, repo string, number int, body string) (commentURL string, err error) {
	if run == nil {
		run = defaultRunner
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty comment body")
	}
	if number <= 0 {
		return "", fmt.Errorf("invalid PR number")
	}
	path, cleanup, err := writeBodyFile(body)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args := []string{"pr", "comment", strconv.Itoa(number), "--body-file", path}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return "", err
	}
	return firstHTTPURL(string(out)), nil
}

func firstHTTPURL(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			return line
		}
	}
	// Sometimes URL is embedded mid-line.
	for field := range strings.FieldsSeq(s) {
		if strings.HasPrefix(field, "https://") || strings.HasPrefix(field, "http://") {
			return field
		}
	}
	return ""
}

// CloseIssue closes a GitHub issue. If body is non-empty, posts it as a comment first.
func CloseIssue(ctx context.Context, repoDir, owner, repo string, number int, body string) error {
	return CloseIssueWith(ctx, defaultRunner, repoDir, owner, repo, number, body)
}

// CloseIssueWith is CloseIssue with an injectable runner.
func CloseIssueWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, body string) error {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return fmt.Errorf("invalid issue number")
	}
	body = strings.TrimSpace(body)
	if body != "" {
		if err := CommentIssueWith(ctx, run, repoDir, owner, repo, number, body); err != nil {
			return err
		}
	}
	args := []string{"issue", "close", strconv.Itoa(number)}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	_, err := run(ctx, repoDir, "gh", args...)
	return err
}

// RequestReviewers adds GitHub PR reviewers by login (host gh auth).
// Equivalent to: gh pr edit N --add-reviewer login[,login…] [--repo owner/repo]
func RequestReviewers(ctx context.Context, repoDir, owner, repo string, number int, logins ...string) error {
	return RequestReviewersWith(ctx, defaultRunner, repoDir, owner, repo, number, logins...)
}

// RequestReviewersWith is RequestReviewers with an injectable runner.
func RequestReviewersWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, logins ...string) error {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return fmt.Errorf("invalid PR number")
	}
	cleaned := make([]string, 0, len(logins))
	for _, l := range logins {
		l = strings.TrimPrefix(strings.TrimSpace(l), "@")
		if l == "" {
			continue
		}
		cleaned = append(cleaned, l)
	}
	if len(cleaned) == 0 {
		return fmt.Errorf("no reviewers")
	}
	args := []string{"pr", "edit", strconv.Itoa(number), "--add-reviewer", strings.Join(cleaned, ",")}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	_, err := run(ctx, repoDir, "gh", args...)
	return err
}

// ClosePR closes a pull request (no comment required).
func ClosePR(ctx context.Context, repoDir, owner, repo string, number int) error {
	return ClosePRWith(ctx, defaultRunner, repoDir, owner, repo, number)
}

// ClosePRWith is ClosePR with an injectable runner.
func ClosePRWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int) error {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return fmt.Errorf("invalid PR number")
	}
	args := []string{"pr", "close", strconv.Itoa(number)}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	_, err := run(ctx, repoDir, "gh", args...)
	return err
}

// MergeOpts controls gh pr merge (never includes bypass flags).
type MergeOpts struct {
	Method        MergeMethod
	AttemptAnyway bool // allow when checks failing; still no --admin
}

// MergePreflight is the pure allow/deny decision before calling gh.
type MergePreflight struct {
	Allow  bool
	Reason string
}

// CheckMergePreflight decides whether a merge may proceed.
// Never authorizes bypass of GitHub branch protection; only gates our call.
// Prefer MergeStateStatus (protection-aware) over Mergeable (git trees only).
func CheckMergePreflight(d PRDetail, attemptAnyway bool) MergePreflight {
	st := strings.ToUpper(strings.TrimSpace(d.State))
	if st != "OPEN" {
		return MergePreflight{Allow: false, Reason: "PR is not OPEN (state=" + st + ")"}
	}
	if d.IsDraft {
		return MergePreflight{Allow: false, Reason: "PR is a draft"}
	}
	if reason := MergeStateBlockReason(d.MergeStateStatus, d.ReviewDecision); reason != "" {
		return MergePreflight{Allow: false, Reason: reason}
	}
	m := strings.ToUpper(strings.TrimSpace(d.Mergeable))
	if m == "CONFLICTING" {
		return MergePreflight{Allow: false, Reason: "PR has merge conflicts"}
	}
	if ChecksFailing(d.Checks) && !attemptAnyway {
		return MergePreflight{Allow: false, Reason: "checks failing; enable attempt anyway to retry plain merge"}
	}
	return MergePreflight{Allow: true}
}

// ChecksFailing reports whether a checks rollup string indicates failures.
func ChecksFailing(checks string) bool {
	c := strings.TrimSpace(checks)
	if c == "" || c == "none" {
		return false
	}
	// SummarizeChecks format: "✓ n · ✗ n · … n"
	return strings.Contains(c, "✗") || strings.Contains(strings.ToLower(c), "fail")
}

// MergePR merges a PR with the given method (default squash). Never passes --admin.
func MergePR(ctx context.Context, repoDir, owner, repo string, number int, opts MergeOpts) error {
	return MergePRWith(ctx, defaultRunner, repoDir, owner, repo, number, opts)
}

// MergePRWith is MergePR with an injectable runner.
func MergePRWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int, opts MergeOpts) error {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return fmt.Errorf("invalid PR number")
	}
	method := NormalizeMergeMethod(string(opts.Method))
	args := []string{"pr", "merge", strconv.Itoa(number), "--" + string(method)}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	// Explicitly never add --admin, --disable-auto, etc. that weaken protection.
	for _, a := range args {
		if a == "--admin" || strings.Contains(a, "bypass") {
			return fmt.Errorf("refusing merge args that bypass protection")
		}
	}
	_, err := run(ctx, repoDir, "gh", args...)
	return err
}

func writeBodyFile(body string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "ghpr-body-*")
	if err != nil {
		return "", func() {}, err
	}
	path = filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}
