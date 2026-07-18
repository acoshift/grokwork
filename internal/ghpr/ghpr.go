// Package ghpr wraps gh CLI for Discord thread PR status cards.
package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Info is a point-in-time pull request snapshot for Discord.
type Info struct {
	Number         int
	URL            string
	Title          string
	State          string // OPEN, MERGED, CLOSED
	IsDraft        bool
	ReviewDecision string
	HeadSHA        string
	HeadRef        string
	Checks         string // human rollup, e.g. "✓ 3 · ✗ 1 · … 2"
}

// Runner runs a command in dir and returns stdout. Tests inject fakes.
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

var defaultRunner Runner = execRunner

func execRunner(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.Bytes()
	if err != nil {
		// gh pr checks exits non-zero when checks fail/pending; still return stdout if present.
		if len(out) > 0 && looksLikeJSON(out) {
			return out, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

func looksLikeJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// githubPRURLRE matches https://github.com/owner/repo/pull/N (optional trailing slash/query).
var githubPRURLRE = regexp.MustCompile(`(?i)https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/(\d+)`)

// ParsedURL is a GitHub PR URL extracted from free text.
type ParsedURL struct {
	URL    string
	Owner  string
	Repo   string
	Number int
}

// ParseGitHubPRURLs returns unique GitHub PR URLs found in text (first occurrence order).
func ParseGitHubPRURLs(text string) []ParsedURL {
	matches := githubPRURLRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]ParsedURL, 0, len(matches))
	seen := map[string]struct{}{}
	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			continue
		}
		u := fmt.Sprintf("https://github.com/%s/%s/pull/%d", m[1], m[2], n)
		key := strings.ToLower(u)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ParsedURL{URL: u, Owner: m[1], Repo: m[2], Number: n})
	}
	return out
}

// IsTerminal reports whether the PR lifecycle is finished for worktree cleanup.
func IsTerminal(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "MERGED", "CLOSED":
		return true
	default:
		return false
	}
}

// DisplayState returns OPEN / DRAFT / MERGED / CLOSED for cards.
func DisplayState(info Info) string {
	st := strings.ToUpper(strings.TrimSpace(info.State))
	if st == "OPEN" && info.IsDraft {
		return "DRAFT"
	}
	if st == "" {
		return "UNKNOWN"
	}
	return st
}

// FormatCard builds the Discord status card body (no embeds).
func FormatCard(info Info) string {
	lines := []string{
		fmt.Sprintf("**PR** · #%d · **%s**", info.Number, DisplayState(info)),
	}
	if t := strings.TrimSpace(info.Title); t != "" {
		lines = append(lines, "**title:** "+truncateRunes(t, 120))
	}
	if c := strings.TrimSpace(info.Checks); c != "" {
		lines = append(lines, "**checks:** "+c)
	} else {
		lines = append(lines, "**checks:** (none)")
	}
	if r := strings.TrimSpace(info.ReviewDecision); r != "" {
		lines = append(lines, "**review:** "+humanReview(r))
	} else {
		lines = append(lines, "**review:** —")
	}
	if u := strings.TrimSpace(info.URL); u != "" {
		lines = append(lines, u)
	}
	return strings.Join(lines, "\n")
}

// FormatStatusLines returns compact lines for @Grok /status.
func FormatStatusLines(info Info) []string {
	if info.Number <= 0 && info.URL == "" {
		return nil
	}
	line := fmt.Sprintf("**pr:** #%d · %s", info.Number, DisplayState(info))
	if info.URL != "" {
		line += " · " + info.URL
	}
	out := []string{line}
	if c := strings.TrimSpace(info.Checks); c != "" {
		out = append(out, "**checks:** "+c)
	}
	if r := strings.TrimSpace(info.ReviewDecision); r != "" {
		out = append(out, "**review:** "+humanReview(r))
	}
	return out
}

func humanReview(r string) string {
	switch strings.ToUpper(strings.TrimSpace(r)) {
	case "APPROVED":
		return "APPROVED"
	case "CHANGES_REQUESTED":
		return "CHANGES_REQUESTED"
	case "REVIEW_REQUIRED":
		return "REVIEW_REQUIRED"
	default:
		return r
	}
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// View loads PR fields by number, URL, or branch (gh pr view selector).
func View(ctx context.Context, repoDir, selector string) (Info, error) {
	return ViewWith(ctx, defaultRunner, repoDir, selector)
}

// ViewWith is View with an injectable runner.
func ViewWith(ctx context.Context, run Runner, repoDir, selector string) (Info, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Info{}, fmt.Errorf("empty PR selector")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "view", selector,
		"--json", "number,url,title,state,isDraft,reviewDecision,headRefOid,headRefName")
	if err != nil {
		return Info{}, err
	}
	info, err := parseViewJSON(raw)
	if err != nil {
		return Info{}, err
	}
	if sum, cErr := ChecksSummaryWith(ctx, run, repoDir, info.Number); cErr == nil {
		info.Checks = sum
	}
	return info, nil
}

// ViewByHead finds a PR for branch (any state) and loads full status.
func ViewByHead(ctx context.Context, repoDir, branch string) (Info, error) {
	return ViewByHeadWith(ctx, defaultRunner, repoDir, branch)
}

// ViewByHeadWith is ViewByHead with an injectable runner.
func ViewByHeadWith(ctx context.Context, run Runner, repoDir, branch string) (Info, error) {
	if run == nil {
		run = defaultRunner
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return Info{}, fmt.Errorf("empty branch")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "list",
		"--head", branch,
		"--state", "all",
		"--json", "number,url,title,state,isDraft,reviewDecision,headRefOid,headRefName",
		"--limit", "5",
	)
	if err != nil {
		return Info{}, err
	}
	var list []viewJSON
	if err := json.Unmarshal(raw, &list); err != nil {
		return Info{}, fmt.Errorf("gh pr list json: %w", err)
	}
	if len(list) == 0 {
		return Info{}, fmt.Errorf("no PR for head %s", branch)
	}
	// Prefer OPEN, then most recently listed.
	pick := list[0]
	for _, p := range list {
		if strings.EqualFold(p.State, "OPEN") {
			pick = p
			break
		}
	}
	info := pick.toInfo()
	if sum, cErr := ChecksSummaryWith(ctx, run, repoDir, info.Number); cErr == nil {
		info.Checks = sum
	}
	return info, nil
}

// ChecksSummary returns a short pass/fail/pending rollup for the PR.
func ChecksSummary(ctx context.Context, repoDir string, number int) (string, error) {
	return ChecksSummaryWith(ctx, defaultRunner, repoDir, number)
}

// ChecksSummaryWith is ChecksSummary with an injectable runner.
func ChecksSummaryWith(ctx context.Context, run Runner, repoDir string, number int) (string, error) {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return "", fmt.Errorf("invalid PR number")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "checks", strconv.Itoa(number),
		"--json", "name,state,bucket")
	if err != nil {
		return "", err
	}
	return SummarizeChecksJSON(raw)
}

type checkRow struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
}

// SummarizeChecksJSON turns gh pr checks --json into a short rollup string.
func SummarizeChecksJSON(raw []byte) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "[]" {
		return "none", nil
	}
	var rows []checkRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("gh pr checks json: %w", err)
	}
	if len(rows) == 0 {
		return "none", nil
	}
	var pass, fail, pending, other int
	for _, r := range rows {
		switch bucketOf(r) {
		case "pass":
			pass++
		case "fail":
			fail++
		case "pending":
			pending++
		default:
			other++
		}
	}
	parts := make([]string, 0, 4)
	if pass > 0 {
		parts = append(parts, fmt.Sprintf("✓ %d", pass))
	}
	if fail > 0 {
		parts = append(parts, fmt.Sprintf("✗ %d", fail))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("… %d", pending))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("· %d", other))
	}
	if len(parts) == 0 {
		return "none", nil
	}
	return strings.Join(parts, " · "), nil
}

func bucketOf(r checkRow) string {
	b := strings.ToLower(strings.TrimSpace(r.Bucket))
	if b != "" {
		switch b {
		case "pass", "fail", "pending", "skipping", "cancel":
			return b
		}
	}
	// Fallback from state when bucket missing.
	st := strings.ToUpper(strings.TrimSpace(r.State))
	switch st {
	case "SUCCESS", "SKIPPED", "NEUTRAL":
		return "pass"
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED":
		return "fail"
	case "PENDING", "QUEUED", "IN_PROGRESS", "EXPECTED":
		return "pending"
	default:
		return "other"
	}
}

type viewJSON struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	ReviewDecision string `json:"reviewDecision"`
	HeadRefOid     string `json:"headRefOid"`
	HeadRefName    string `json:"headRefName"`
}

func (v viewJSON) toInfo() Info {
	return Info{
		Number:         v.Number,
		URL:            v.URL,
		Title:          v.Title,
		State:          strings.ToUpper(strings.TrimSpace(v.State)),
		IsDraft:        v.IsDraft,
		ReviewDecision: v.ReviewDecision,
		HeadSHA:        v.HeadRefOid,
		HeadRef:        v.HeadRefName,
	}
}

func parseViewJSON(raw []byte) (Info, error) {
	var v viewJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return Info{}, fmt.Errorf("gh pr view json: %w", err)
	}
	if v.Number <= 0 {
		return Info{}, fmt.Errorf("gh pr view: missing number")
	}
	return v.toInfo(), nil
}
