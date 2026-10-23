package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// MaxPRComments is how many of a PR's newest comments the detail view keeps.
const MaxPRComments = 50

// PRDetail is a richer PR snapshot for the web ship surface (beyond status cards).
type PRDetail struct {
	Info
	Body string
	// Comments is the PR conversation (issue comments), oldest first, capped at
	// the newest MaxPRComments. Review comments are a different surface and are
	// not included here.
	Comments []IssueComment
	// CommentsOmitted counts older comments dropped by that cap.
	CommentsOmitted  int
	Mergeable        string // MERGEABLE, CONFLICTING, UNKNOWN, … (git trees only)
	MergeStateStatus string // CLEAN, BLOCKED, DIRTY, BEHIND, UNSTABLE, HAS_HOOKS, UNKNOWN
	BaseRef          string
	Author           string
	Additions        int
	Deletions        int
	ChangedFiles     int
	Truncated        bool
}

// prDetailFields is the gh pr view --json list for the detail page.
// comments is intentionally absent: requesting it makes gh page through the
// whole thread (preloadPrComments, 100 full bodies per round trip) and this
// view keeps only the newest MaxPRComments. Those come from one GraphQL
// comments(last:) call instead.
const prDetailFields = "number,url,title,state,isDraft,reviewDecision,headRefOid,headRefName,baseRefName,body,mergeable,mergeStateStatus,author,additions,deletions,changedFiles"

// PRDetailOptions selects the parts of a snapshot that cost an extra round trip.
type PRDetailOptions struct {
	// Comments loads the newest MaxPRComments issue comments.
	// The gates strip leaves this false: it does not render the conversation.
	Comments bool
}

// ViewPRDetail loads PR fields including body and merge metadata.
func ViewPRDetail(ctx context.Context, repoDir, selector string) (PRDetail, error) {
	return ViewPRDetailWith(ctx, defaultRunner, repoDir, selector)
}

// ViewPRDetailWith is ViewPRDetail with an injectable runner.
// It includes the conversation.
func ViewPRDetailWith(ctx context.Context, run Runner, repoDir, selector string) (PRDetail, error) {
	return ViewPRDetailOpts(ctx, run, repoDir, selector, PRDetailOptions{Comments: true})
}

// ViewPRDetailOpts is ViewPRDetailWith with options.
// The PR view and the checks rollup run together; neither waits on the other.
func ViewPRDetailOpts(ctx context.Context, run Runner, repoDir, selector string, opts PRDetailOptions) (PRDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return PRDetail{}, fmt.Errorf("empty PR selector")
	}

	var (
		raw     []byte
		viewErr error
		checks  string
	)
	viewDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(viewDone)
		raw, viewErr = run(ctx, repoDir, "gh", "pr", "view", selector, "--json", prDetailFields)
	})
	wg.Go(func() {
		sum, err := ChecksSummaryWith(ctx, run, repoDir, selector)
		if err == nil {
			checks = sum
		}
	})
	// Comments only need the view (owner/repo/number). Start them as soon as
	// that returns so they overlap whatever is left of the checks call.
	<-viewDone
	if viewErr != nil {
		wg.Wait()
		return PRDetail{}, viewErr
	}
	d, commentsInPayload, err := parsePRDetailJSON(raw)
	if err != nil {
		wg.Wait()
		return PRDetail{}, err
	}
	fillOwnerRepo(&d.Info)
	if opts.Comments && !commentsInPayload {
		wg.Go(func() {
			// The header and gates are already loaded. A comment failure must
			// not blank the page the way a failed `gh pr view` does. Checks
			// use the same rule.
			_ = attachPRComments(ctx, run, repoDir, selector, &d)
		})
	}
	wg.Wait()
	d.Checks = checks
	return d, nil
}

type prDetailJSON struct {
	Number           int             `json:"number"`
	URL              string          `json:"url"`
	Title            string          `json:"title"`
	State            string          `json:"state"`
	IsDraft          bool            `json:"isDraft"`
	ReviewDecision   string          `json:"reviewDecision"`
	HeadRefOid       string          `json:"headRefOid"`
	HeadRefName      string          `json:"headRefName"`
	BaseRefName      string          `json:"baseRefName"`
	Body             string          `json:"body"`
	Comments         json.RawMessage `json:"comments"`
	Mergeable        string          `json:"mergeable"`
	MergeStateStatus string          `json:"mergeStateStatus"`
	Author           any             `json:"author"`
	Additions        int             `json:"additions"`
	Deletions        int             `json:"deletions"`
	ChangedFiles     int             `json:"changedFiles"`
}

type prCommentJSON struct {
	Author    any    `json:"author"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

func parsePRDetailJSON(raw []byte) (PRDetail, bool, error) {
	var j prDetailJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return PRDetail{}, false, fmt.Errorf("gh pr view detail json: %w", err)
	}
	body, trunc := truncateBytes(j.Body, DefaultIssueBodyCap)
	d := PRDetail{
		Info: Info{
			Number:         j.Number,
			URL:            j.URL,
			Title:          j.Title,
			State:          j.State,
			IsDraft:        j.IsDraft,
			ReviewDecision: j.ReviewDecision,
			HeadSHA:        j.HeadRefOid,
			HeadRef:        j.HeadRefName,
		},
		Body:             body,
		Mergeable:        j.Mergeable,
		MergeStateStatus: j.MergeStateStatus,
		BaseRef:          j.BaseRefName,
		Author:           authorLogin(j.Author),
		Additions:        j.Additions,
		Deletions:        j.Deletions,
		ChangedFiles:     j.ChangedFiles,
		Truncated:        trunc,
	}
	comments, present, err := parsePRCommentField(j.Comments)
	if err != nil {
		return PRDetail{}, false, err
	}
	if present {
		d.Comments, d.CommentsOmitted = capPRComments(comments, len(comments))
	}
	return d, present, nil
}

// parsePRCommentField reports whether the payload included comments.
// A missing field is not the same as an empty list: gh omits the key when
// the view did not ask for it, and that is when the bounded query runs.
func parsePRCommentField(raw json.RawMessage) ([]IssueComment, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, true, nil
	}
	var src []prCommentJSON
	if err := json.Unmarshal(trimmed, &src); err != nil {
		return nil, false, fmt.Errorf("gh pr view detail json: %w", err)
	}
	return issueCommentsFromJSON(src), true, nil
}

func issueCommentsFromJSON(src []prCommentJSON) []IssueComment {
	if len(src) == 0 {
		return nil
	}
	out := make([]IssueComment, 0, len(src))
	for _, c := range src {
		// Truncated stays about the description. A long comment must not make
		// the page claim the body was cut.
		cb, _ := truncateBytes(c.Body, DefaultIssueBodyCap)
		out = append(out, IssueComment{
			Author:    authorLogin(c.Author),
			Body:      cb,
			URL:       c.URL,
			CreatedAt: parseGHTime(c.CreatedAt),
		})
	}
	return out
}

// capPRComments keeps the newest MaxPRComments. total is the full thread
// length, which may be larger than len(src) when src is already a tail window.
// Comments stay oldest-first.
func capPRComments(src []IssueComment, total int) ([]IssueComment, int) {
	if total < len(src) {
		total = len(src)
	}
	if len(src) > MaxPRComments {
		src = src[len(src)-MaxPRComments:]
	}
	omitted := total - len(src)
	if omitted < 0 {
		omitted = 0
	}
	return src, omitted
}

func attachPRComments(ctx context.Context, run Runner, repoDir, selector string, d *PRDetail) error {
	owner, repo, number := d.Owner, d.Repo, d.Number
	if owner == "" || repo == "" || number <= 0 {
		o, r, n := parsePRSelector(selector)
		if owner == "" {
			owner = o
		}
		if repo == "" {
			repo = r
		}
		if number <= 0 {
			number = n
		}
	}
	if owner == "" || repo == "" || number <= 0 {
		return nil
	}
	comments, total, err := prCommentsLast(ctx, run, repoDir, owner, repo, number, MaxPRComments)
	if err != nil {
		return err
	}
	d.Comments, d.CommentsOmitted = capPRComments(comments, total)
	return nil
}

func parsePRSelector(selector string) (owner, repo string, number int) {
	m := githubPRURLRE.FindStringSubmatch(selector)
	if len(m) < 4 {
		return "", "", 0
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n
}

const prCommentsLastQuery = `
query($owner: String!, $repo: String!, $number: Int!, $last: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      comments(last: $last) {
        totalCount
        nodes {
          author { login }
          body
          url
          createdAt
        }
      }
    }
  }
}`

func prCommentsLast(ctx context.Context, run Runner, repoDir, owner, repo string, number, last int) ([]IssueComment, int, error) {
	if last <= 0 {
		last = MaxPRComments
	}
	args := []string{
		"api", "graphql",
		"-f", "query=" + strings.TrimSpace(prCommentsLastQuery),
		"-F", "owner=" + owner,
		"-F", "repo=" + repo,
		"-F", "number=" + strconv.Itoa(number),
		"-F", "last=" + strconv.Itoa(last),
	}
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, 0, fmt.Errorf("pr comments: %w", err)
	}
	return parsePRCommentsLast(out)
}

type prCommentsEnvelope struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				Comments struct {
					TotalCount int             `json:"totalCount"`
					Nodes      []prCommentJSON `json:"nodes"`
				} `json:"comments"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func parsePRCommentsLast(raw []byte) ([]IssueComment, int, error) {
	var env prCommentsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, fmt.Errorf("parse pr comments: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, 0, fmt.Errorf("graphql: %s", env.Errors[0].Message)
	}
	if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
		return nil, 0, fmt.Errorf("pull request not found")
	}
	c := env.Data.Repository.PullRequest.Comments
	return issueCommentsFromJSON(c.Nodes), c.TotalCount, nil
}

// MergeStateBlocksMerge reports statuses that must refuse a plain merge.
// Empty is not a block (field absent / legacy). UNKNOWN is a block — GitHub
// is still computing, so we do not claim ship-ready or call merge yet.
func MergeStateBlocksMerge(status string) bool {
	return MergeStateBlockReason(status, "") != ""
}

// MergeStateBlockReason returns a user-facing refuse reason for a blocking
// mergeStateStatus, or "" if the status alone does not block. reviewDecision
// refines BLOCKED (required reviews vs generic protection).
func MergeStateBlockReason(status, reviewDecision string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "BLOCKED":
		switch strings.ToUpper(strings.TrimSpace(reviewDecision)) {
		case "REVIEW_REQUIRED":
			return "GitHub requires an approving review before merge"
		case "CHANGES_REQUESTED":
			return "GitHub merge is blocked (changes requested)"
		default:
			return "GitHub merge is blocked (branch protection or required reviews)"
		}
	case "DIRTY":
		return "PR has merge conflicts"
	case "BEHIND":
		return "branch is behind the base; update required"
	case "HAS_HOOKS":
		return "required status checks or hooks not satisfied"
	case "UNKNOWN":
		return "merge status still computing; wait and retry"
	default:
		return ""
	}
}

// MergeStateAllowsShip is true when the status is green enough for the PR
// ship strip "ready" affordance. Empty keeps legacy mergeable-only logic
// (field absent). UNKNOWN is not ready — still computing.
func MergeStateAllowsShip(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "", "CLEAN", "UNSTABLE":
		return true
	default:
		return false
	}
}
