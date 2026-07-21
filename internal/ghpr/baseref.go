package ghpr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Common base branch names for worktree / completion diffs. Prefer remote
// tracking refs (origin/…) so local stale branches don't win.
var defaultBaseRefCandidates = []string{
	"origin/main",
	"origin/master",
	"origin/prod",
	"origin/production",
	"origin/staging",
	"origin/stag",
	"origin/develop",
	"origin/dev",
	"main",
	"master",
	"prod",
	"staging",
	"stag",
	"develop",
}

// PreferOriginRef maps a short branch name (e.g. "prod" from a PR base) to
// origin/prod when that ref exists, else the local name, else origin/<name>.
func PreferOriginRef(ctx context.Context, run Runner, cwd, name string) string {
	if run == nil {
		run = defaultRunner
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "HEAD" {
		return name
	}
	if strings.Contains(name, "/") {
		return name
	}
	origin := "origin/" + name
	if refExists(ctx, run, cwd, origin) {
		return origin
	}
	if refExists(ctx, run, cwd, name) {
		return name
	}
	return origin
}

// ResolveDiffBaseRef picks the left-side base for a worktree diff.
// preferred is optional (PR baseRefName like "prod"); when usable it wins.
// Otherwise the closest existing candidate to HEAD is chosen (fewest commits
// ahead of merge-base), so backports onto prod are not diffed against main.
func ResolveDiffBaseRef(ctx context.Context, run Runner, cwd, preferred string) string {
	if run == nil {
		run = defaultRunner
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "HEAD"
	}
	if p := strings.TrimSpace(preferred); p != "" && p != "HEAD" {
		ref := PreferOriginRef(ctx, run, cwd, p)
		if _, err := run(ctx, cwd, "git", "merge-base", ref, "HEAD"); err == nil {
			return ref
		}
	}
	if d := DetectClosestBaseRef(ctx, run, cwd); d != "" {
		return d
	}
	return "HEAD"
}

// DetectClosestBaseRef returns the candidate base whose merge-base with HEAD
// is nearest (minimal rev-list count merge-base..HEAD). Empty if none work.
func DetectClosestBaseRef(ctx context.Context, run Runner, cwd string) string {
	if run == nil {
		run = defaultRunner
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	best := ""
	bestAhead := -1
	for _, c := range defaultBaseRefCandidates {
		if !refExists(ctx, run, cwd, c) {
			continue
		}
		mbOut, err := run(ctx, cwd, "git", "merge-base", c, "HEAD")
		if err != nil {
			continue
		}
		mb := strings.TrimSpace(string(mbOut))
		if mb == "" {
			continue
		}
		nOut, err := run(ctx, cwd, "git", "rev-list", "--count", mb+"..HEAD")
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(nOut)))
		if err != nil || n < 0 {
			continue
		}
		if bestAhead < 0 || n < bestAhead {
			bestAhead = n
			best = c
		}
	}
	return best
}

// PRBaseRefWith returns baseRefName from `gh pr view` (e.g. "prod", "main").
func PRBaseRefWith(ctx context.Context, run Runner, repoDir, selector string) (string, error) {
	if run == nil {
		run = defaultRunner
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("empty PR selector")
	}
	raw, err := run(ctx, repoDir, "gh", "pr", "view", selector, "--json", "baseRefName")
	if err != nil {
		return "", err
	}
	var j struct {
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return "", fmt.Errorf("gh pr view baseRefName json: %w", err)
	}
	b := strings.TrimSpace(j.BaseRefName)
	if b == "" {
		return "", fmt.Errorf("empty baseRefName for %s", selector)
	}
	return b, nil
}

func refExists(ctx context.Context, run Runner, cwd, ref string) bool {
	_, err := run(ctx, cwd, "git", "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}
