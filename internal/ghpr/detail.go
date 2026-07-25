package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PRDetail is a richer PR snapshot for the web ship surface (beyond status cards).
type PRDetail struct {
	Info
	Body             string
	Mergeable        string // MERGEABLE, CONFLICTING, UNKNOWN, … (git trees only)
	MergeStateStatus string // CLEAN, BLOCKED, DIRTY, BEHIND, UNSTABLE, HAS_HOOKS, UNKNOWN
	BaseRef          string
	Author           string
	Additions        int
	Deletions        int
	ChangedFiles     int
	Truncated        bool
}

// ViewPRDetail loads PR fields including body and merge metadata.
func ViewPRDetail(ctx context.Context, repoDir, selector string) (PRDetail, error) {
	return ViewPRDetailWith(ctx, defaultRunner, repoDir, selector)
}

// ViewPRDetailWith is ViewPRDetail with an injectable runner.
func ViewPRDetailWith(ctx context.Context, run Runner, repoDir, selector string) (PRDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return PRDetail{}, fmt.Errorf("empty PR selector")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "view", selector,
		"--json", "number,url,title,state,isDraft,reviewDecision,headRefOid,headRefName,baseRefName,body,mergeable,mergeStateStatus,author,additions,deletions,changedFiles")
	if err != nil {
		return PRDetail{}, err
	}
	d, err := parsePRDetailJSON(raw)
	if err != nil {
		return PRDetail{}, err
	}
	fillOwnerRepo(&d.Info)
	sel := d.URL
	if sel == "" {
		sel = selector
	}
	if sum, cErr := ChecksSummaryWith(ctx, run, repoDir, sel); cErr == nil {
		d.Checks = sum
	}
	return d, nil
}

type prDetailJSON struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	Title            string `json:"title"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	ReviewDecision   string `json:"reviewDecision"`
	HeadRefOid       string `json:"headRefOid"`
	HeadRefName      string `json:"headRefName"`
	BaseRefName      string `json:"baseRefName"`
	Body             string `json:"body"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Author           any    `json:"author"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	ChangedFiles     int    `json:"changedFiles"`
}

func parsePRDetailJSON(raw []byte) (PRDetail, error) {
	var j prDetailJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return PRDetail{}, fmt.Errorf("gh pr view detail json: %w", err)
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
	return d, nil
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
