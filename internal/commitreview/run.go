package commitreview

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/grokrun"
)

// GrokRunner abstracts grokrun.Run for tests.
type GrokRunner func(ctx context.Context, opt grokrun.Options) grokrun.Result

// IssueCreator abstracts ghpr.CreateIssueWith for tests.
type IssueCreator func(ctx context.Context, repoDir, owner, repo string, opts ghpr.CreateIssueOpts) (number int, url string, err error)

// Read-only tool allowlist for review: gather context without shell or edits.
// MCP meta-tools are denied separately via ExtraArgs.
const ReviewTools = "read_file,grep,list_dir"

// DefaultMaxTurns allows a few read-only lookups plus a final JSON answer.
const DefaultMaxTurns = 25

// DefaultTimeout bounds a context-heavy review (tools + large patches).
const DefaultTimeout = 12 * time.Minute

// Deps wires external systems for Run.
type Deps struct {
	Store     *Store
	Grok      GrokRunner
	Create    IssueCreator
	Git       ghpr.Runner
	GrokBin   string
	Model     string
	Timeout   time.Duration
	MaxTurns  int
}

// StartOpts is input for starting a review job (async caller sets StatusQueued then calls Execute).
type StartOpts struct {
	Project  string
	Owner    string
	Repo     string
	SHA      string
	Cwd      string
	Actor    string
	Subject  string
	ShortSHA string
}

// Execute runs review + issue create for an existing job (status queued).
// It updates the store as it progresses. Safe to call from a goroutine.
func Execute(ctx context.Context, deps Deps, job *Job, cwd string) {
	if job == nil || deps.Store == nil {
		return
	}
	if deps.Grok == nil {
		deps.Grok = grokrun.Run
	}
	if deps.Create == nil {
		deps.Create = func(ctx context.Context, repoDir, owner, repo string, opts ghpr.CreateIssueOpts) (int, string, error) {
			return ghpr.CreateIssueWith(ctx, deps.Git, repoDir, owner, repo, opts)
		}
	}
	if deps.Timeout <= 0 {
		deps.Timeout = DefaultTimeout
	}
	if deps.MaxTurns <= 0 {
		deps.MaxTurns = DefaultMaxTurns
	}

	job.Status = StatusRunning
	job.Error = ""
	_ = deps.Store.Save(job)

	detail, err := ghpr.ShowCommitWith(ctx, deps.Git, cwd, job.SHA, ghpr.DiffCaps{})
	if err != nil {
		fail(deps.Store, job, fmt.Errorf("git show: %w", err))
		return
	}
	job.SHA = detail.SHA
	job.ShortSHA = detail.ShortSHA
	job.Subject = detail.Subject
	_ = deps.Store.Save(job)

	prompt := BuildPrompt(detail, MaxFindings)
	tools := ReviewTools
	res := deps.Grok(ctx, grokrun.Options{
		GrokBin:          deps.GrokBin,
		Prompt:           prompt,
		Cwd:              cwd,
		Yolo:             false,
		Model:            deps.Model,
		MaxTurns:         deps.MaxTurns,
		Timeout:          deps.Timeout,
		Tools:            &tools,
		NoSubagents:      true,
		NoPlan:           true,
		NoMemory:         true,
		DisableWebSearch: true,
		JSONSchema:       FindingsJSONSchema,
		// Allowlist still leaves MCP meta-tools; block them so review stays local.
		ExtraArgs: []string{"--deny", "MCPTool"},
	})
	if res.Cancelled {
		fail(deps.Store, job, fmt.Errorf("review cancelled"))
		return
	}
	if res.Code != 0 && strings.TrimSpace(res.Text) == "" {
		fail(deps.Store, job, fmt.Errorf("grok failed code=%d: %s", res.Code, truncate(res.Stderr, 300)))
		return
	}

	summary, findings, err := ParseFindings(res.Text)
	if err != nil {
		if res.MaxTurnsReached || isMaxTurnsStderr(res.Stderr) {
			fail(deps.Store, job, fmt.Errorf("review hit max turns without findings JSON: %w; model said: %s", err, truncate(res.Text, 240)))
			return
		}
		fail(deps.Store, job, fmt.Errorf("parse findings: %w; model said: %s", err, truncate(res.Text, 240)))
		return
	}
	job.Summary = summary
	job.Findings = findings
	job.Status = StatusCreatingIssues
	_ = deps.Store.Save(job)

	if len(findings) == 0 {
		job.Status = StatusDone
		_ = deps.Store.Save(job)
		return
	}

	for i := range job.Findings {
		f := &job.Findings[i]
		title := IssueTitle(job.ShortSHA, f.Title)
		body := IssueBody(job, *f)
		labels := DefaultLabels(f.Severity, f.Labels)
		n, url, cerr := deps.Create(ctx, cwd, job.Owner, job.Repo, ghpr.CreateIssueOpts{
			Title:  title,
			Body:   body,
			Labels: labels,
		})
		if cerr != nil {
			f.CreateError = cerr.Error()
		} else {
			f.IssueNumber = n
			f.IssueURL = url
		}
		_ = deps.Store.Save(job)
	}

	// Partial create failures still mark done; UI shows per-finding errors.
	job.Status = StatusDone
	_ = deps.Store.Save(job)
}

func fail(store *Store, job *Job, err error) {
	job.Status = StatusFailed
	job.Error = err.Error()
	_ = store.Save(job)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isMaxTurnsStderr(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "max turns reached") || strings.Contains(s, "max_turns_reached")
}

// NewQueuedJob builds a queued job ready to save + execute.
func NewQueuedJob(opts StartOpts) *Job {
	now := time.Now().UTC()
	sha := strings.TrimSpace(opts.SHA)
	short := strings.TrimSpace(opts.ShortSHA)
	if short == "" {
		if len(sha) >= 7 {
			short = sha[:7]
		} else {
			short = sha
		}
	}
	return &Job{
		ID:        NewJobID(),
		CreatedAt: now,
		UpdatedAt: now,
		Actor:     opts.Actor,
		Project:   opts.Project,
		Owner:     opts.Owner,
		Repo:      opts.Repo,
		SHA:       sha,
		ShortSHA:  short,
		Subject:   opts.Subject,
		Status:    StatusQueued,
	}
}
