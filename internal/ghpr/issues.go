package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultIssueBodyCap is the max body/comment size kept for prompts/UI.
const DefaultIssueBodyCap = 32 * 1024

// IssueListOpts controls gh issue list.
type IssueListOpts struct {
	// Owner/Repo force --repo owner/repo (recommended for multi-repo projects).
	Owner string
	Repo  string
	// State: open (default), closed, all.
	State string
	// Limit defaults to 30.
	Limit int
	// Labels optional filter.
	Labels []string
}

// IssueComment is one issue comment.
type IssueComment struct {
	Author string
	Body   string
	URL    string
}

// IssueLinkedPR is a pull request linked as a closing reference for an issue
// (GitHub "Development" / closedByPullRequestsReferences).
type IssueLinkedPR struct {
	Number  int
	URL     string
	Title   string
	State   string // OPEN, CLOSED, MERGED
	IsDraft bool
	Owner   string
	Repo    string
}

// IssueInfo is a GitHub issue snapshot for web/read surfaces.
type IssueInfo struct {
	Number    int
	URL       string
	Title     string
	State     string // OPEN, CLOSED
	Body      string
	Author    string
	Labels    []string
	Comments  []IssueComment
	LinkedPRs []IssueLinkedPR
	Owner     string
	Repo      string
	CreatedAt time.Time
	UpdatedAt time.Time
	Truncated bool // body or comments hit size caps
	// WorkState is set by the web layer (not gh): "FIXING" when a non-terminal
	// Grok session binds this issue with Fixes. Empty when none.
	WorkState string
}

// ListIssues lists issues for a repo via gh.
func ListIssues(ctx context.Context, repoDir string, opts IssueListOpts) ([]IssueInfo, error) {
	return ListIssuesWith(ctx, defaultRunner, repoDir, opts)
}

// ListIssuesWith is ListIssues with an injectable runner.
func ListIssuesWith(ctx context.Context, run Runner, repoDir string, opts IssueListOpts) ([]IssueInfo, error) {
	if run == nil {
		run = defaultRunner
	}
	state := strings.ToLower(strings.TrimSpace(opts.State))
	if state == "" {
		state = "open"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	// List UI needs metadata only — omit body/labels (large; detail uses ViewIssue).
	args := []string{"issue", "list",
		"--state", state,
		"--limit", strconv.Itoa(limit),
		"--json", "number,url,title,state,author,createdAt,updatedAt,closedByPullRequestsReferences",
	}
	if o, r := strings.TrimSpace(opts.Owner), strings.TrimSpace(opts.Repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	for _, lab := range opts.Labels {
		lab = strings.TrimSpace(lab)
		if lab != "" {
			args = append(args, "--label", lab)
		}
	}
	raw, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, err
	}
	return parseIssueListJSON(raw, opts.Owner, opts.Repo)
}

// ViewIssue loads one issue including comments (body capped).
func ViewIssue(ctx context.Context, repoDir string, number int) (IssueInfo, error) {
	return ViewIssueWith(ctx, defaultRunner, repoDir, number, "", "")
}

// ViewIssueWith loads an issue; owner/repo optional --repo override.
// When owner/repo are set, linked PRs are enriched via GraphQL (title, state).
func ViewIssueWith(ctx context.Context, run Runner, repoDir string, number int, owner, repo string) (IssueInfo, error) {
	if run == nil {
		run = defaultRunner
	}
	if number <= 0 {
		return IssueInfo{}, fmt.Errorf("invalid issue number")
	}
	args := []string{"issue", "view", strconv.Itoa(number),
		"--json", "number,url,title,state,author,labels,body,comments,createdAt,updatedAt,closedByPullRequestsReferences",
	}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	raw, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return IssueInfo{}, err
	}
	info, err := parseIssueViewJSON(raw, owner, repo, DefaultIssueBodyCap)
	if err != nil {
		return IssueInfo{}, err
	}
	// Prefer GraphQL for title/state on linked PRs (gh --json omits them).
	if info.Owner != "" && info.Repo != "" && len(info.LinkedPRs) > 0 {
		if rich, gErr := listIssueLinkedPRsWith(ctx, run, repoDir, info.Owner, info.Repo, number); gErr == nil && len(rich) > 0 {
			info.LinkedPRs = rich
		}
	}
	return info, nil
}

type issueLinkedPRJSON struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	Repository *struct {
		Name  string `json:"name"`
		Owner *struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

type issueJSON struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Author    any    `json:"author"` // {login} or string
	Labels    []any  `json:"labels"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Comments  []struct {
		Author any    `json:"author"`
		Body   string `json:"body"`
		URL    string `json:"url"`
	} `json:"comments"`
	ClosedByPullRequestsReferences []issueLinkedPRJSON `json:"closedByPullRequestsReferences"`
}

func parseIssueListJSON(raw []byte, owner, repo string) ([]IssueInfo, error) {
	var rows []issueJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("gh issue list json: %w", err)
	}
	out := make([]IssueInfo, 0, len(rows))
	for _, r := range rows {
		info, _ := r.toInfo(owner, repo, DefaultIssueBodyCap)
		out = append(out, info)
	}
	return out, nil
}

func parseIssueViewJSON(raw []byte, owner, repo string, bodyCap int) (IssueInfo, error) {
	var row issueJSON
	if err := json.Unmarshal(raw, &row); err != nil {
		return IssueInfo{}, fmt.Errorf("gh issue view json: %w", err)
	}
	return row.toInfo(owner, repo, bodyCap)
}

func (r issueJSON) toInfo(owner, repo string, bodyCap int) (IssueInfo, error) {
	info := IssueInfo{
		Number:    r.Number,
		URL:       r.URL,
		Title:     r.Title,
		State:     strings.ToUpper(strings.TrimSpace(r.State)),
		Author:    authorLogin(r.Author),
		Owner:     strings.TrimSpace(owner),
		Repo:      strings.TrimSpace(repo),
		CreatedAt: parseGHTime(r.CreatedAt),
		UpdatedAt: parseGHTime(r.UpdatedAt),
	}
	body, trunc := truncateBytes(r.Body, bodyCap)
	info.Body = body
	info.Truncated = trunc
	for _, lab := range r.Labels {
		info.Labels = append(info.Labels, labelName(lab))
	}
	for _, c := range r.Comments {
		cb, ct := truncateBytes(c.Body, bodyCap)
		if ct {
			info.Truncated = true
		}
		info.Comments = append(info.Comments, IssueComment{
			Author: authorLogin(c.Author),
			Body:   cb,
			URL:    c.URL,
		})
	}
	for _, pr := range r.ClosedByPullRequestsReferences {
		info.LinkedPRs = append(info.LinkedPRs, pr.toLinkedPR(info.Owner, info.Repo))
	}
	if info.Owner == "" || info.Repo == "" {
		fillIssueOwnerRepo(&info)
		// Backfill empty owner/repo on linked PRs after URL parse.
		for i := range info.LinkedPRs {
			if info.LinkedPRs[i].Owner == "" {
				info.LinkedPRs[i].Owner = info.Owner
			}
			if info.LinkedPRs[i].Repo == "" {
				info.LinkedPRs[i].Repo = info.Repo
			}
		}
	}
	return info, nil
}

func (p issueLinkedPRJSON) toLinkedPR(fallbackOwner, fallbackRepo string) IssueLinkedPR {
	pr := IssueLinkedPR{
		Number:  p.Number,
		URL:     strings.TrimSpace(p.URL),
		Title:   strings.TrimSpace(p.Title),
		State:   strings.ToUpper(strings.TrimSpace(p.State)),
		IsDraft: p.IsDraft,
		Owner:   strings.TrimSpace(fallbackOwner),
		Repo:    strings.TrimSpace(fallbackRepo),
	}
	if p.Repository != nil {
		if n := strings.TrimSpace(p.Repository.Name); n != "" {
			pr.Repo = n
		}
		if p.Repository.Owner != nil {
			if o := strings.TrimSpace(p.Repository.Owner.Login); o != "" {
				pr.Owner = o
			}
		}
	}
	if pr.Owner == "" || pr.Repo == "" {
		fillLinkedPROwnerRepo(&pr)
	}
	if pr.URL == "" && pr.Owner != "" && pr.Repo != "" && pr.Number > 0 {
		pr.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.Owner, pr.Repo, pr.Number)
	}
	return pr
}

func fillLinkedPROwnerRepo(pr *IssueLinkedPR) {
	const prefix = "https://github.com/"
	u := strings.TrimSpace(pr.URL)
	if !strings.HasPrefix(strings.ToLower(u), prefix) {
		return
	}
	rest := u[len(prefix):]
	parts := strings.Split(rest, "/")
	// owner/repo/pull/N
	if len(parts) < 2 {
		return
	}
	if pr.Owner == "" {
		pr.Owner = parts[0]
	}
	if pr.Repo == "" {
		pr.Repo = parts[1]
	}
}

// listIssueLinkedPRsWith loads closing-reference PRs with title/state via GraphQL.
func listIssueLinkedPRsWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int) ([]IssueLinkedPR, error) {
	if run == nil {
		run = defaultRunner
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" || number <= 0 {
		return nil, fmt.Errorf("owner, repo, and positive issue number required")
	}
	const query = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $number) {
      closedByPullRequestsReferences(first: 30, includeClosedPrs: true) {
        nodes {
          number
          title
          url
          state
          isDraft
          repository { name owner { login } }
        }
      }
    }
  }
}`
	args := []string{
		"api", "graphql",
		"-f", "query=" + strings.TrimSpace(query),
		"-F", "owner=" + owner,
		"-F", "repo=" + repo,
		"-F", "number=" + strconv.Itoa(number),
	}
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("list issue linked PRs: %w", err)
	}
	return parseIssueLinkedPRsGraphQL(out, owner, repo)
}

type gqlIssueLinkedPRsEnvelope struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				ClosedByPullRequestsReferences struct {
					Nodes []issueLinkedPRJSON `json:"nodes"`
				} `json:"closedByPullRequestsReferences"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func parseIssueLinkedPRsGraphQL(raw []byte, fallbackOwner, fallbackRepo string) ([]IssueLinkedPR, error) {
	var env gqlIssueLinkedPRsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse issue linked PRs: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", env.Errors[0].Message)
	}
	if env.Data.Repository == nil || env.Data.Repository.Issue == nil {
		return nil, fmt.Errorf("issue not found")
	}
	nodes := env.Data.Repository.Issue.ClosedByPullRequestsReferences.Nodes
	out := make([]IssueLinkedPR, 0, len(nodes))
	for _, n := range nodes {
		if n.Number <= 0 {
			continue
		}
		out = append(out, n.toLinkedPR(fallbackOwner, fallbackRepo))
	}
	return out, nil
}

func fillIssueOwnerRepo(info *IssueInfo) {
	// https://github.com/owner/repo/issues/N
	const prefix = "https://github.com/"
	u := strings.TrimSpace(info.URL)
	if !strings.HasPrefix(strings.ToLower(u), prefix) {
		return
	}
	rest := u[len(prefix):]
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return
	}
	if info.Owner == "" {
		info.Owner = parts[0]
	}
	if info.Repo == "" {
		info.Repo = parts[1]
	}
}

func authorLogin(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["login"].(string); ok {
			return s
		}
	}
	return ""
}

// parseGHTime parses GitHub JSON timestamps (RFC3339 / RFC3339Nano).
func parseGHTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func labelName(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["name"].(string); ok {
			return s
		}
	}
	return fmt.Sprint(v)
}

func truncateBytes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	// Prefer rune-safe cut near max.
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	// max is bytes; approximate with runes under max bytes.
	cut := max
	if cut > len(r) {
		cut = len(r)
	}
	// Walk until byte length near max.
	out := string(r)
	if len(out) <= max {
		return out, false
	}
	for cut > 0 && len(string(r[:cut])) > max-1 {
		cut--
	}
	if cut < 1 {
		cut = 1
	}
	return string(r[:cut]) + "…", true
}
