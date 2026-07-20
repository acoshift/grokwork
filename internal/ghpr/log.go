package ghpr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Commit list defaults.
const (
	DefaultCommitListLimit = 50
	MaxCommitListLimit     = 100
)

// CommitSummary is one row from git log.
type CommitSummary struct {
	SHA         string
	ShortSHA    string
	Subject     string
	AuthorName  string
	AuthorEmail string
	AuthorDate  time.Time
}

// CommitDetail is git show metadata plus optional structured patch.
type CommitDetail struct {
	CommitSummary
	Body string
	Stat string
	Diff Diff
}

// CommitListOpts controls git log.
type CommitListOpts struct {
	// Ref is a branch, tag, or SHA (default HEAD).
	Ref string
	// Limit is max commits (default DefaultCommitListLimit, max MaxCommitListLimit).
	Limit int
}

// ListCommits runs git log on the main checkout.
func ListCommits(ctx context.Context, cwd string, opts CommitListOpts) ([]CommitSummary, error) {
	return ListCommitsWith(ctx, defaultRunner, cwd, opts)
}

// ListCommitsWith is ListCommits with an injectable runner.
func ListCommitsWith(ctx context.Context, run Runner, cwd string, opts CommitListOpts) ([]CommitSummary, error) {
	if run == nil {
		run = defaultRunner
	}
	ref := strings.TrimSpace(opts.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultCommitListLimit
	}
	if limit > MaxCommitListLimit {
		limit = MaxCommitListLimit
	}
	// Fields separated by unit separator (0x1f); subject is single-line (%s).
	const format = "%H%x1f%s%x1f%an%x1f%ae%x1f%aI"
	raw, err := run(ctx, cwd, "git", "log", "--format="+format, "-n", strconv.Itoa(limit), ref)
	if err != nil {
		return nil, err
	}
	return parseCommitLog(raw)
}

func parseCommitLog(raw []byte) ([]CommitSummary, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	out := make([]CommitSummary, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x1f")
		if len(parts) < 5 {
			return nil, fmt.Errorf("git log: unexpected line %q", line)
		}
		c := CommitSummary{
			SHA:         strings.TrimSpace(parts[0]),
			Subject:     parts[1],
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[4])); err == nil {
			c.AuthorDate = t
		}
		c.ShortSHA = shortSHA(c.SHA)
		if c.SHA == "" {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ShowCommit runs git show for one commit (stat + patch, capped).
func ShowCommit(ctx context.Context, cwd, sha string, caps DiffCaps) (CommitDetail, error) {
	return ShowCommitWith(ctx, defaultRunner, cwd, sha, caps)
}

// ShowCommitWith is ShowCommit with an injectable runner.
func ShowCommitWith(ctx context.Context, run Runner, cwd, sha string, caps DiffCaps) (CommitDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return CommitDetail{}, fmt.Errorf("empty commit sha")
	}
	// Resolve to full SHA first.
	full, err := run(ctx, cwd, "git", "rev-parse", "--verify", sha+"^{commit}")
	if err != nil {
		return CommitDetail{}, err
	}
	fullSHA := strings.TrimSpace(string(full))
	if fullSHA == "" {
		return CommitDetail{}, fmt.Errorf("commit not found: %s", sha)
	}

	// Metadata: hash, subject, author, email, date, body (after blank line in %B we use separate).
	const metaFmt = "%H%x1f%s%x1f%an%x1f%ae%x1f%aI%x1f%b"
	metaRaw, err := run(ctx, cwd, "git", "show", "-s", "--format="+metaFmt, fullSHA)
	if err != nil {
		return CommitDetail{}, err
	}
	metaLine := strings.TrimRight(string(metaRaw), "\n")
	parts := strings.SplitN(metaLine, "\x1f", 6)
	if len(parts) < 5 {
		return CommitDetail{}, fmt.Errorf("git show: unexpected metadata")
	}
	detail := CommitDetail{
		CommitSummary: CommitSummary{
			SHA:         strings.TrimSpace(parts[0]),
			ShortSHA:    shortSHA(strings.TrimSpace(parts[0])),
			Subject:     parts[1],
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
		},
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[4])); err == nil {
		detail.AuthorDate = t
	}
	if len(parts) >= 6 {
		detail.Body = strings.TrimSpace(parts[5])
	}

	statRaw, err := run(ctx, cwd, "git", "show", "--format=", "--stat", "--no-ext-diff", fullSHA)
	if err != nil {
		return CommitDetail{}, err
	}
	detail.Stat = strings.TrimSpace(string(statRaw))

	patchRaw, err := run(ctx, cwd, "git", "show", "--format=", "-p", "--no-ext-diff", fullSHA)
	if err != nil {
		return CommitDetail{}, err
	}
	detail.Diff = ParseUnifiedDiff(patchRaw, caps)
	return detail, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}
