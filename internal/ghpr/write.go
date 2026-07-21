package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
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
	// Prefer JSON for stable parse; fall back handled by parseCreateIssueOutput.
	args = append(args, "--json", "number,url")
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		// Retry without labels if labels caused failure (missing label in repo).
		if len(opts.Labels) > 0 {
			argsNoLabel := []string{"issue", "create", "--title", title, "--body-file", path}
			if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
				argsNoLabel = append(argsNoLabel, "--repo", o+"/"+r)
			}
			argsNoLabel = append(argsNoLabel, "--json", "number,url")
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
	if run == nil {
		run = defaultRunner
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("empty comment body")
	}
	if number <= 0 {
		return fmt.Errorf("invalid PR number")
	}
	path, cleanup, err := writeBodyFile(body)
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"pr", "comment", strconv.Itoa(number), "--body-file", path}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	_, err = run(ctx, repoDir, "gh", args...)
	return err
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
	Method         MergeMethod
	AttemptAnyway  bool // allow when checks failing; still no --admin
}

// MergePreflight is the pure allow/deny decision before calling gh.
type MergePreflight struct {
	Allow  bool
	Reason string
}

// CheckMergePreflight decides whether a merge may proceed.
// Never authorizes bypass of GitHub branch protection; only gates our call.
func CheckMergePreflight(state, mergeable, checks string, attemptAnyway bool) MergePreflight {
	st := strings.ToUpper(strings.TrimSpace(state))
	if st != "OPEN" {
		return MergePreflight{Allow: false, Reason: "PR is not OPEN (state=" + st + ")"}
	}
	m := strings.ToUpper(strings.TrimSpace(mergeable))
	if m == "CONFLICTING" {
		return MergePreflight{Allow: false, Reason: "PR has merge conflicts"}
	}
	if ChecksFailing(checks) && !attemptAnyway {
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
