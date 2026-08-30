package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultPRListLimit is how many open PRs one import/list call asks GitHub for.
const DefaultPRListLimit = 40

// PRListOpts controls gh pr list.
type PRListOpts struct {
	// Owner/Repo force --repo owner/repo (required — cwd remotes must not decide).
	Owner string
	Repo  string
	// State: open (default), closed, merged, all.
	State string
	// Limit defaults to DefaultPRListLimit.
	Limit int
}

// PRListItem is one row from gh pr list (no checks — View fills those).
type PRListItem struct {
	Info
	Author    string
	UpdatedAt time.Time
}

// ListPRs lists pull requests for a repo via gh.
func ListPRs(ctx context.Context, repoDir string, opts PRListOpts) ([]PRListItem, error) {
	return ListPRsWith(ctx, defaultRunner, repoDir, opts)
}

// ListPRsWith is ListPRs with an injectable runner.
func ListPRsWith(ctx context.Context, run Runner, repoDir string, opts PRListOpts) ([]PRListItem, error) {
	if run == nil {
		run = defaultRunner
	}
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("gh pr list requires owner/repo")
	}
	state := strings.ToLower(strings.TrimSpace(opts.State))
	if state == "" {
		state = "open"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPRListLimit
	}
	args := []string{"pr", "list",
		"--state", state,
		"--limit", strconv.Itoa(limit),
		"--repo", owner + "/" + repo,
		"--json", "number,url,title,state,isDraft,author,reviewDecision,headRefOid,headRefName,updatedAt",
	}
	raw, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, err
	}
	return parsePRListJSON(raw, owner, repo)
}

type prListJSON struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	Author         any    `json:"author"`
	ReviewDecision string `json:"reviewDecision"`
	HeadRefOid     string `json:"headRefOid"`
	HeadRefName    string `json:"headRefName"`
	UpdatedAt      string `json:"updatedAt"`
}

func parsePRListJSON(raw []byte, owner, repo string) ([]PRListItem, error) {
	var rows []prListJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("gh pr list json: %w", err)
	}
	out := make([]PRListItem, 0, len(rows))
	for _, r := range rows {
		item := r.toItem(owner, repo)
		if item.Number <= 0 {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (r prListJSON) toItem(owner, repo string) PRListItem {
	item := PRListItem{
		Info: Info{
			Number:         r.Number,
			URL:            r.URL,
			Title:          r.Title,
			State:          strings.ToUpper(strings.TrimSpace(r.State)),
			IsDraft:        r.IsDraft,
			ReviewDecision: r.ReviewDecision,
			HeadSHA:        r.HeadRefOid,
			HeadRef:        r.HeadRefName,
			Owner:          strings.TrimSpace(owner),
			Repo:           strings.TrimSpace(repo),
		},
		Author:    authorLogin(r.Author),
		UpdatedAt: parseGHTime(r.UpdatedAt),
	}
	fillOwnerRepo(&item.Info)
	if item.URL == "" && item.Owner != "" && item.Repo != "" && item.Number > 0 {
		item.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", item.Owner, item.Repo, item.Number)
	}
	return item
}
