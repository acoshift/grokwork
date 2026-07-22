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
	// DefaultCommitListLimit is the commits browser page size.
	DefaultCommitListLimit = 50
	// MaxCommitListLimit caps a single git log page size (safety bound).
	MaxCommitListLimit = 2000
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
	// Skip is how many commits to skip from the start of the log (pagination).
	Skip int
}

// Fetch updates remote-tracking branches in cwd (git fetch --all --prune).
// Shallow clones are converted to full history (--unshallow) so the commits
// browser can load the complete log. Does not move local branch tips or touch
// the working tree.
func Fetch(ctx context.Context, cwd string) error {
	return FetchWith(ctx, defaultRunner, cwd)
}

// FetchWith is Fetch with an injectable runner.
func FetchWith(ctx context.Context, run Runner, cwd string) error {
	if run == nil {
		run = defaultRunner
	}
	if strings.TrimSpace(cwd) == "" {
		return fmt.Errorf("empty repo path")
	}
	args := []string{"fetch", "--all", "--prune"}
	if isShallowRepo(ctx, run, cwd) {
		// --unshallow fails on complete repos; only add when needed.
		args = append(args, "--unshallow")
	}
	_, err := run(ctx, cwd, "git", args...)
	if err != nil {
		return err
	}
	return nil
}

// isShallowRepo reports whether cwd is a shallow clone.
// Unknown/error → false so fetch keeps the safe non-unshallow path.
func isShallowRepo(ctx context.Context, run Runner, cwd string) bool {
	out, err := run(ctx, cwd, "git", "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
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
	skip := opts.Skip
	if skip < 0 {
		skip = 0
	}
	// Fields separated by unit separator (0x1f); subject is single-line (%s).
	const format = "%H%x1f%s%x1f%an%x1f%ae%x1f%aI"
	args := []string{"log", "--format=" + format, "-n", strconv.Itoa(limit)}
	if skip > 0 {
		args = append(args, "--skip", strconv.Itoa(skip))
	}
	args = append(args, ref)
	raw, err := run(ctx, cwd, "git", args...)
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

// ShowCommitMetaWith loads commit metadata only (no stat, no patch) — for
// pages that list files and fetch hunks per file.
func ShowCommitMetaWith(ctx context.Context, run Runner, cwd, sha string) (CommitDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	return showCommitMeta(ctx, run, cwd, sha)
}

// ShowCommitWith is ShowCommit with an injectable runner.
func ShowCommitWith(ctx context.Context, run Runner, cwd, sha string, caps DiffCaps) (CommitDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	detail, err := showCommitMeta(ctx, run, cwd, sha)
	if err != nil {
		return CommitDetail{}, err
	}
	fullSHA := detail.SHA

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

func showCommitMeta(ctx context.Context, run Runner, cwd, sha string) (CommitDetail, error) {
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
	return detail, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}
