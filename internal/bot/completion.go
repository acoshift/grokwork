package bot

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/gitworktree"
)

// DefaultRiskyPathGlobs flags paths that usually need careful review.
// Patterns use ** (any path prefix/suffix) and * (within one segment).
var DefaultRiskyPathGlobs = []string{
	"**/migrations/**",
	"**/migration/**",
	"**/*migration*",
	"**/auth/**",
	"**/deploy/**",
	"**/deployment/**",
	"**/.env",
	"**/.env.*",
	"**/secrets/**",
	"**/*secret*",
	"**/*credential*",
	"**/Dockerfile*",
	"**/*.tf",
	"**/k8s/**",
	"**/helm/**",
	"**/crdb/**",
	"**/gcp.json",
}

const (
	maxCompletionNameLines = 12
	maxCompletionMsgRunes  = 1800
)

// DiffSummary is a deterministic git snapshot for the completion card.
type DiffSummary struct {
	Branch     string
	HeadShort  string
	BaseRef    string // e.g. origin/main
	Stat       string // --stat body (may be empty)
	NameStatus []string
	Insertions int
	Deletions  int
	FileCount  int
	Dirty      bool
	DirtyStat  string
	Risky      []string
	HasCommits bool // HEAD is ahead of merge-base
}

// CollectDiffSummary runs git in cwd against a detected base branch.
func CollectDiffSummary(ctx context.Context, cwd string, riskyGlobs []string) (DiffSummary, error) {
	if cwd == "" || !gitworktree.IsRepo(cwd) {
		return DiffSummary{}, fmt.Errorf("not a git repo")
	}
	if len(riskyGlobs) == 0 {
		riskyGlobs = DefaultRiskyPathGlobs
	}

	var out DiffSummary
	out.Branch, _ = gitOutput(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if out.Branch == "HEAD" {
		out.Branch = ""
	}
	if head, err := gitOutput(ctx, cwd, "rev-parse", "--short", "HEAD"); err == nil {
		out.HeadShort = head
	}

	base, err := detectBaseRef(ctx, cwd)
	if err != nil {
		// Still report dirty working tree if any.
		base = ""
	}
	out.BaseRef = base

	if base != "" {
		rangeSpec := base + "...HEAD"
		if stat, sErr := gitOutput(ctx, cwd, "diff", "--stat", rangeSpec); sErr == nil {
			out.Stat = strings.TrimSpace(stat)
			out.Insertions, out.Deletions, out.FileCount = parseDiffStatSummary(out.Stat)
		}
		if names, nErr := gitOutput(ctx, cwd, "diff", "--name-status", rangeSpec); nErr == nil {
			out.NameStatus = parseNameStatus(names)
			if out.FileCount == 0 {
				out.FileCount = len(out.NameStatus)
			}
		}
		// Commits on branch vs base?
		if n, cErr := gitOutput(ctx, cwd, "rev-list", "--count", base+"..HEAD"); cErr == nil {
			out.HasCommits = strings.TrimSpace(n) != "0" && strings.TrimSpace(n) != ""
		}
	}

	if porcelain, pErr := gitOutput(ctx, cwd, "status", "--porcelain"); pErr == nil && strings.TrimSpace(porcelain) != "" {
		out.Dirty = true
		if ds, dErr := gitOutput(ctx, cwd, "diff", "--stat", "HEAD"); dErr == nil {
			out.DirtyStat = strings.TrimSpace(ds)
		}
		// Include untracked / dirty paths in name list and risk scan.
		for _, line := range strings.Split(porcelain, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// XY<path> or XY path -> path (rename: "R  a -> b")
			path := porcelainPath(line)
			if path == "" {
				continue
			}
			out.NameStatus = appendUniqueStatus(out.NameStatus, "?", path)
		}
	}

	out.Risky = filterRiskyPaths(pathsFromNameStatus(out.NameStatus), riskyGlobs)
	return out, nil
}

func detectBaseRef(ctx context.Context, cwd string) (string, error) {
	candidates := []string{"origin/main", "origin/master", "main", "master"}
	for _, c := range candidates {
		if _, err := gitOutput(ctx, cwd, "rev-parse", "--verify", c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no base branch found")
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// parseDiffStatSummary reads the last line of git diff --stat ("N files changed, X insertions(+), Y deletions(-)").
func parseDiffStatSummary(stat string) (ins, del, files int) {
	stat = strings.TrimSpace(stat)
	if stat == "" {
		return 0, 0, 0
	}
	lines := strings.Split(stat, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	// "3 files changed, 10 insertions(+), 2 deletions(-)"
	// "1 file changed, 1 insertion(+)"
	re := regexp.MustCompile(`(\d+)\s+files?\s+changed(?:.*?(\d+)\s+insertions?\(\+\))?(?:.*?(\d+)\s+deletions?\(-\))?`)
	m := re.FindStringSubmatch(last)
	if m == nil {
		return 0, 0, 0
	}
	fmt.Sscanf(m[1], "%d", &files)
	if m[2] != "" {
		fmt.Sscanf(m[2], "%d", &ins)
	}
	if m[3] != "" {
		fmt.Sscanf(m[3], "%d", &del)
	}
	return ins, del, files
}

func parseNameStatus(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// M\tpath  or  R100\told\tnew
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		if len(status) > 0 {
			status = string(status[0]) // M, A, D, R, C, …
		}
		path := parts[len(parts)-1]
		out = append(out, status+"\t"+path)
	}
	return out
}

func porcelainPath(line string) string {
	if len(line) < 3 {
		return ""
	}
	// Format: XY PATH or XY ORIG -> PATH
	rest := strings.TrimSpace(line[2:])
	if i := strings.Index(rest, " -> "); i >= 0 {
		return strings.TrimSpace(rest[i+4:])
	}
	return rest
}

func appendUniqueStatus(list []string, status, path string) []string {
	entry := status + "\t" + path
	for _, e := range list {
		if strings.HasSuffix(e, "\t"+path) || e == entry {
			return list
		}
	}
	return append(list, entry)
}

func pathsFromNameStatus(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for _, e := range entries {
		parts := strings.SplitN(e, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		p := parts[1]
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func filterRiskyPaths(paths []string, globs []string) []string {
	if len(paths) == 0 || len(globs) == 0 {
		return nil
	}
	compiled := make([]*regexp.Regexp, 0, len(globs))
	for _, g := range globs {
		if re, err := pathGlobRegexp(g); err == nil {
			compiled = append(compiled, re)
		}
	}
	var risky []string
	for _, p := range paths {
		slash := filepath.ToSlash(p)
		for _, re := range compiled {
			if re.MatchString(slash) {
				risky = append(risky, p)
				break
			}
		}
	}
	return risky
}

// pathGlobRegexp compiles a simple ** / * path glob to a case-insensitive regexp.
func pathGlobRegexp(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(strings.TrimSpace(glob))
	if glob == "" {
		return nil, fmt.Errorf("empty glob")
	}
	var b strings.Builder
	b.WriteString("(?i)^")
	for i := 0; i < len(glob); {
		if strings.HasPrefix(glob[i:], "**/") {
			b.WriteString("(.*/)?")
			i += 3
			continue
		}
		if strings.HasPrefix(glob[i:], "**") {
			b.WriteString(".*")
			i += 2
			continue
		}
		if glob[i] == '*' {
			b.WriteString("[^/]*")
			i++
			continue
		}
		if glob[i] == '?' {
			b.WriteString("[^/]")
			i++
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(glob[i])))
		i++
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()), nil
}

// CompletionCardInput is everything needed to format the post-run summary.
type CompletionCardInput struct {
	Status   string // Done / Cancelled / Finished with exit N
	Project  string
	Elapsed  time.Duration
	Branch   string
	PRURL    string
	PRNumber int
	Diff     DiffSummary
	Queued   int
}

// FormatCompletionCard builds the Discord completion summary (no embeds).
// Returns empty string when there is nothing useful to show (no git changes).
func FormatCompletionCard(in CompletionCardInput) string {
	d := in.Diff
	if !d.HasCommits && !d.Dirty && d.FileCount == 0 && len(d.NameStatus) == 0 {
		return ""
	}

	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "Done"
	}

	var lines []string
	head := fmt.Sprintf("**Summary** · %s · **%s** · %s", status, in.Project, formatElapsed(in.Elapsed))
	lines = append(lines, head)

	branch := in.Branch
	if branch == "" {
		branch = d.Branch
	}
	if branch != "" || d.HeadShort != "" {
		b := branch
		if b == "" {
			b = "(detached)"
		}
		if d.HeadShort != "" {
			lines = append(lines, fmt.Sprintf("**branch:** `%s` @ `%s`", b, d.HeadShort))
		} else {
			lines = append(lines, fmt.Sprintf("**branch:** `%s`", b))
		}
	}
	if d.BaseRef != "" {
		lines = append(lines, "**base:** `"+d.BaseRef+"`")
	}

	switch {
	case d.FileCount > 0 || d.Insertions > 0 || d.Deletions > 0:
		lines = append(lines, fmt.Sprintf("**diff:** %d file%s · +%d -%d",
			d.FileCount, plural(d.FileCount), d.Insertions, d.Deletions))
	case d.Dirty:
		lines = append(lines, "**diff:** uncommitted changes")
	case d.HasCommits:
		lines = append(lines, "**diff:** commits present (stat unavailable)")
	}

	if names := formatNameStatusLines(d.NameStatus, maxCompletionNameLines); names != "" {
		lines = append(lines, "```")
		lines = append(lines, names)
		lines = append(lines, "```")
	}
	if d.Dirty && d.DirtyStat != "" && d.Stat == "" {
		// Only show dirty stat when committed stat was empty.
		trimmed := truncateRunes(d.DirtyStat, 400)
		lines = append(lines, "**working tree:**")
		lines = append(lines, "```")
		lines = append(lines, trimmed)
		lines = append(lines, "```")
	}
	if len(d.Risky) > 0 {
		shown := d.Risky
		extra := 0
		if len(shown) > 6 {
			extra = len(shown) - 6
			shown = shown[:6]
		}
		risk := strings.Join(shown, ", ")
		if extra > 0 {
			risk += fmt.Sprintf(" (+%d more)", extra)
		}
		lines = append(lines, "**risk:** "+risk)
	}
	if in.PRURL != "" {
		if in.PRNumber > 0 {
			lines = append(lines, fmt.Sprintf("**pr:** #%d · %s", in.PRNumber, in.PRURL))
		} else {
			lines = append(lines, "**pr:** "+in.PRURL)
		}
	}
	if in.Queued > 0 {
		lines = append(lines, fmt.Sprintf("**queue:** %d follow-up%s", in.Queued, plural(in.Queued)))
	}

	text := strings.Join(lines, "\n")
	return truncateRunes(text, maxCompletionMsgRunes)
}

func formatNameStatusLines(entries []string, maxLines int) string {
	if len(entries) == 0 || maxLines <= 0 {
		return ""
	}
	var b strings.Builder
	n := len(entries)
	limit := maxLines
	if n < limit {
		limit = n
	}
	for i := 0; i < limit; i++ {
		parts := strings.SplitN(entries[i], "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s %s", parts[0], parts[1])
	}
	if n > limit {
		fmt.Fprintf(&b, "\n… +%d more", n-limit)
	}
	return b.String()
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

// postCompletionSummary collects git diff info and posts a summary card.
func (b *Bot) postCompletionSummary(s *discordgo.Session, threadID, project, cwd, branch string, elapsed time.Duration, resultCode int, cancelled bool) {
	if s == nil || threadID == "" || cancelled {
		return
	}
	if cwd == "" || !gitworktree.IsRepo(cwd) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	diff, err := CollectDiffSummary(ctx, cwd, b.riskyPathGlobs())
	if err != nil {
		log.Printf("completion: diff thread=%s: %v", threadID, err)
		return
	}

	status := "Done"
	switch {
	case cancelled:
		status = "Cancelled"
	case resultCode != 0:
		status = fmt.Sprintf("Exit %d", resultCode)
	}

	prURL, prNum := "", 0
	if e, ok := b.sessions.Get(threadID); ok {
		prURL = e.PRURL
		prNum = e.PRNumber
		if branch == "" {
			branch = e.WorktreeBranch
		}
	}

	card := FormatCompletionCard(CompletionCardInput{
		Status:   status,
		Project:  project,
		Elapsed:  elapsed,
		Branch:   branch,
		PRURL:    prURL,
		PRNumber: prNum,
		Diff:     diff,
		Queued:   b.queueLen(threadID),
	})
	if card == "" {
		log.Printf("completion: no code changes thread=%s", threadID)
		return
	}
	if _, err := discordSend(s, threadID, card); err != nil {
		log.Printf("completion: send thread=%s: %v", threadID, err)
		return
	}
	log.Printf("completion: posted thread=%s files=%d risk=%d", threadID, diff.FileCount, len(diff.Risky))
}

func (b *Bot) riskyPathGlobs() []string {
	if b == nil || b.cfg == nil {
		return DefaultRiskyPathGlobs
	}
	if !b.cfg.RiskyPathGlobsConfigured() {
		return DefaultRiskyPathGlobs
	}
	// Explicit list (possibly empty = disable risk flags).
	return b.cfg.RiskyPathGlobsEffective()
}
