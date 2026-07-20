package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ReviewComment is one unresolved PR review comment / thread leaf for Address review.
type ReviewComment struct {
	Path   string
	Line   int
	Body   string
	Author string
	URL    string
}

// ListUnresolvedReviewComments lists unresolved review threads on a PR via gh GraphQL.
func ListUnresolvedReviewComments(ctx context.Context, repoDir, owner, repo string, number int) ([]ReviewComment, error) {
	return ListUnresolvedReviewCommentsWith(ctx, defaultRunner, repoDir, owner, repo, number)
}

// ListUnresolvedReviewCommentsWith is the injectable-runner variant.
func ListUnresolvedReviewCommentsWith(ctx context.Context, run Runner, repoDir, owner, repo string, number int) ([]ReviewComment, error) {
	if run == nil {
		run = defaultRunner
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" || number <= 0 {
		return nil, fmt.Errorf("owner, repo, and positive PR number required")
	}
	const query = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          isOutdated
          path
          line
          comments(first: 30) {
            nodes {
              body
              url
              author { login }
            }
          }
        }
      }
    }
  }
}`
	// gh api graphql -f query=... -F owner= -F repo= -F number=
	args := []string{
		"api", "graphql",
		"-f", "query=" + strings.TrimSpace(query),
		"-F", "owner=" + owner,
		"-F", "repo=" + repo,
		"-F", "number=" + strconv.Itoa(number),
	}
	out, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("list review comments: %w", err)
	}
	return parseReviewThreadsJSON(out)
}

type gqlReviewEnvelope struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads struct {
					Nodes []struct {
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       *int   `json:"line"`
						Comments   struct {
							Nodes []struct {
								Body   string `json:"body"`
								URL    string `json:"url"`
								Author *struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func parseReviewThreadsJSON(raw []byte) ([]ReviewComment, error) {
	var env gqlReviewEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse review threads: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", env.Errors[0].Message)
	}
	if env.Data.Repository == nil || env.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("pull request not found")
	}
	var out []ReviewComment
	for _, th := range env.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if th.IsResolved {
			continue
		}
		// Prefer the latest comment in the thread as the actionable note.
		if len(th.Comments.Nodes) == 0 {
			continue
		}
		c := th.Comments.Nodes[len(th.Comments.Nodes)-1]
		author := ""
		if c.Author != nil {
			author = c.Author.Login
		}
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		out = append(out, ReviewComment{
			Path:   th.Path,
			Line:   line,
			Body:   c.Body,
			Author: author,
			URL:    c.URL,
		})
	}
	return out, nil
}
