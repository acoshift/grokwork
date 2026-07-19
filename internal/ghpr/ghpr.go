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
	Owner          string
	Repo           string
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
	label := fmt.Sprintf("#%d", info.Number)
	if info.Owner != "" && info.Repo != "" {
		label = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
	}
	lines := []string{
		fmt.Sprintf("**PR** · %s · **%s**", label, DisplayState(info)),
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
	label := fmt.Sprintf("#%d", info.Number)
	if info.Owner != "" && info.Repo != "" {
		label = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
	}
	line := fmt.Sprintf("**pr:** %s · %s", label, DisplayState(info))
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

// FormatMultiStatusLines lists several PRs for @Grok /status.
func FormatMultiStatusLines(infos []Info) []string {
	if len(infos) == 0 {
		return nil
	}
	if len(infos) == 1 {
		return FormatStatusLines(infos[0])
	}
	out := []string{fmt.Sprintf("**prs:** %d tracked", len(infos))}
	for _, info := range infos {
		label := fmt.Sprintf("#%d", info.Number)
		if info.Owner != "" && info.Repo != "" {
			label = fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
		}
		line := fmt.Sprintf("• %s · %s", label, DisplayState(info))
		if c := strings.TrimSpace(info.Checks); c != "" {
			line += " · " + c
		}
		if info.URL != "" {
			line += " · " + info.URL
		}
		out = append(out, line)
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
	fillOwnerRepo(&info)
	sel := info.URL
	if sel == "" {
		sel = selector
	}
	if sum, cErr := ChecksSummaryWith(ctx, run, repoDir, sel); cErr == nil {
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
	fillOwnerRepo(&info)
	sel := info.URL
	if sel == "" {
		sel = strconv.Itoa(info.Number)
	}
	if sum, cErr := ChecksSummaryWith(ctx, run, repoDir, sel); cErr == nil {
		info.Checks = sum
	}
	return info, nil
}

// Check is one CI status row from gh pr checks.
type Check struct {
	Name   string
	State  string
	Bucket string // pass, fail, pending, skipping, cancel
	Link   string
}

// ChecksSummary returns a short pass/fail/pending rollup for the PR.
// selector is a PR number, URL, or branch (passed to gh pr checks).
func ChecksSummary(ctx context.Context, repoDir string, selector string) (string, error) {
	return ChecksSummaryWith(ctx, defaultRunner, repoDir, selector)
}

// ChecksSummaryWith is ChecksSummary with an injectable runner.
func ChecksSummaryWith(ctx context.Context, run Runner, repoDir, selector string) (string, error) {
	checks, err := ListChecksWith(ctx, run, repoDir, selector)
	if err != nil {
		return "", err
	}
	return SummarizeChecks(checks), nil
}

// ListChecks returns all check rows for a PR (number or URL selector).
func ListChecks(ctx context.Context, repoDir, selector string) ([]Check, error) {
	return ListChecksWith(ctx, defaultRunner, repoDir, selector)
}

// ListChecksWith is ListChecks with an injectable runner.
func ListChecksWith(ctx context.Context, run Runner, repoDir, selector string) ([]Check, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("empty PR selector")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "checks", selector,
		"--json", "name,state,bucket,link")
	if err != nil {
		return nil, err
	}
	return ParseChecksJSON(raw)
}

func fillOwnerRepo(info *Info) {
	if info == nil {
		return
	}
	m := githubPRURLRE.FindStringSubmatch(info.URL)
	if len(m) < 4 {
		return
	}
	if info.Owner == "" {
		info.Owner = m[1]
	}
	if info.Repo == "" {
		info.Repo = m[2]
	}
}

type checkRow struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
	Link   string `json:"link"`
}

// ParseChecksJSON parses gh pr checks --json output.
func ParseChecksJSON(raw []byte) ([]Check, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "[]" {
		return nil, nil
	}
	var rows []checkRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("gh pr checks json: %w", err)
	}
	out := make([]Check, 0, len(rows))
	for _, r := range rows {
		out = append(out, Check{
			Name:   r.Name,
			State:  r.State,
			Bucket: bucketOf(r),
			Link:   r.Link,
		})
	}
	return out, nil
}

// SummarizeChecksJSON turns gh pr checks --json into a short rollup string.
func SummarizeChecksJSON(raw []byte) (string, error) {
	checks, err := ParseChecksJSON(raw)
	if err != nil {
		return "", err
	}
	return SummarizeChecks(checks), nil
}

// SummarizeChecks builds the card rollup (✓ / ✗ / …).
func SummarizeChecks(checks []Check) string {
	if len(checks) == 0 {
		return "none"
	}
	var pass, fail, pending, other int
	for _, r := range checks {
		switch strings.ToLower(r.Bucket) {
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
		return "none"
	}
	return strings.Join(parts, " · ")
}

// HasFailing reports whether any check is in the fail bucket.
func HasFailing(checks []Check) bool {
	for _, c := range checks {
		if strings.EqualFold(c.Bucket, "fail") {
			return true
		}
	}
	return false
}

// FailedChecks returns only failing check rows.
func FailedChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if strings.EqualFold(c.Bucket, "fail") {
			out = append(out, c)
		}
	}
	return out
}

// FormatCIDigest builds a Discord CI failure notice (no embeds).
func FormatCIDigest(prNumber int, headSHA string, failed []Check) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**CI failed** · PR #%d", prNumber)
	if headSHA != "" {
		short := headSHA
		if len(short) > 7 {
			short = short[:7]
		}
		fmt.Fprintf(&b, " · `%s`", short)
	}
	b.WriteByte('\n')
	if len(failed) == 0 {
		b.WriteString("One or more checks failed.\n")
	} else {
		limit := 8
		for i, c := range failed {
			if i >= limit {
				fmt.Fprintf(&b, "… +%d more\n", len(failed)-limit)
				break
			}
			name := strings.TrimSpace(c.Name)
			if name == "" {
				name = "(unnamed check)"
			}
			if c.Link != "" {
				fmt.Fprintf(&b, "• **%s** — %s\n", name, c.Link)
			} else {
				fmt.Fprintf(&b, "• **%s**\n", name)
			}
		}
	}
	b.WriteString("Fix: `@Grok /fix-ci`")
	return strings.TrimSpace(b.String())
}

// FailedLogSnippet fetches truncated failed-job logs for a branch (best-effort).
func FailedLogSnippet(ctx context.Context, repoDir, branch, headSHA string, maxRunes int) string {
	return FailedLogSnippetWith(ctx, defaultRunner, repoDir, branch, headSHA, maxRunes)
}

// FailedLogSnippetWith is FailedLogSnippet with an injectable runner.
func FailedLogSnippetWith(ctx context.Context, run Runner, repoDir, branch, headSHA string, maxRunes int) string {
	if run == nil {
		run = defaultRunner
	}
	if maxRunes <= 0 {
		maxRunes = 1500
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	raw, err := run(ctx, repoDir, "gh", "run", "list",
		"--branch", branch,
		"--limit", "10",
		"--json", "databaseId,conclusion,headSha,status,name",
	)
	if err != nil {
		return ""
	}
	var runs []struct {
		DatabaseID int64  `json:"databaseId"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"headSha"`
		Status     string `json:"status"`
		Name       string `json:"name"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) == 0 {
		return ""
	}
	headSHA = strings.TrimSpace(headSHA)
	shaMatch := func(runSHA string) bool {
		if headSHA == "" || runSHA == "" {
			return true
		}
		return strings.HasPrefix(runSHA, headSHA) || strings.HasPrefix(headSHA, runSHA) || strings.EqualFold(runSHA, headSHA)
	}
	var runID int64
	for _, r := range runs {
		if !strings.EqualFold(r.Conclusion, "failure") && !strings.EqualFold(r.Conclusion, "timed_out") {
			continue
		}
		if !shaMatch(r.HeadSHA) {
			continue
		}
		runID = r.DatabaseID
		break
	}
	if runID == 0 {
		// Fall back to first failed run on the branch.
		for _, r := range runs {
			if strings.EqualFold(r.Conclusion, "failure") || strings.EqualFold(r.Conclusion, "timed_out") {
				runID = r.DatabaseID
				break
			}
		}
	}
	if runID == 0 {
		return ""
	}
	logRaw, err := run(ctx, repoDir, "gh", "run", "view", strconv.FormatInt(runID, 10), "--log-failed")
	if err != nil || len(logRaw) == 0 {
		return ""
	}
	return tailRunes(string(logRaw), maxRunes)
}

func tailRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…\n" + string(r[len(r)-n:])
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
	info := Info{
		Number:         v.Number,
		URL:            v.URL,
		Title:          v.Title,
		State:          strings.ToUpper(strings.TrimSpace(v.State)),
		IsDraft:        v.IsDraft,
		ReviewDecision: v.ReviewDecision,
		HeadSHA:        v.HeadRefOid,
		HeadRef:        v.HeadRefName,
	}
	fillOwnerRepo(&info)
	return info
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
