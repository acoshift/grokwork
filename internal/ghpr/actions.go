package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Default run-list page size when Limit is unset.
const DefaultRunListLimit = 20

// DefaultJobLogRunes caps JobLog output when maxRunes is unset.
const DefaultJobLogRunes = 60_000

// WorkflowInfo is one row from gh workflow list.
type WorkflowInfo struct {
	ID    int64
	Name  string
	Path  string
	State string // active, disabled_manually, …
}

// RunInfo is one row from gh run list.
type RunInfo struct {
	ID           int64
	Number       int
	Attempt      int
	Title        string
	WorkflowName string
	WorkflowID   int64
	Branch       string
	HeadSHA      string
	Event        string
	Status       string
	Conclusion   string
	URL          string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RunListOpts filters gh run list.
type RunListOpts struct {
	// WorkflowID, when >0, restricts to that workflow (gh --workflow <id>).
	WorkflowID int64
	// Branch restricts to runs on that branch tip.
	Branch string
	// Limit defaults to DefaultRunListLimit when <=0.
	Limit int
}

// RunStep is one step inside a workflow job.
type RunStep struct {
	Name       string
	Status     string
	Conclusion string
	Number     int
}

// RunJob is one job inside a workflow run.
type RunJob struct {
	ID          int64
	Name        string
	Status      string
	Conclusion  string
	URL         string
	StartedAt   time.Time
	CompletedAt time.Time
	Steps       []RunStep
}

// RunDetail is gh run view metadata plus jobs/steps.
type RunDetail struct {
	ID           int64
	Attempt      int
	Title        string
	WorkflowName string
	Branch       string
	HeadSHA      string
	Event        string
	Status       string
	Conclusion   string
	URL          string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Jobs         []RunJob
}

// ListWorkflows lists workflows for the repo at repoDir (including disabled).
func ListWorkflows(ctx context.Context, repoDir string) ([]WorkflowInfo, error) {
	return ListWorkflowsWith(ctx, defaultRunner, repoDir)
}

// ListWorkflowsWith is ListWorkflows with an injectable runner.
// Equivalent to: gh workflow list --all --json id,name,path,state
func ListWorkflowsWith(ctx context.Context, run Runner, repoDir string) ([]WorkflowInfo, error) {
	if run == nil {
		run = defaultRunner
	}
	raw, err := run(ctx, repoDir, "gh", "workflow", "list", "--all",
		"--json", "id,name,path,state")
	if err != nil {
		return nil, err
	}
	return ParseWorkflowsJSON(raw)
}

// ListRuns lists recent workflow runs.
func ListRuns(ctx context.Context, repoDir string, opts RunListOpts) ([]RunInfo, error) {
	return ListRunsWith(ctx, defaultRunner, repoDir, opts)
}

// ListRunsWith is ListRuns with an injectable runner.
// Equivalent to: gh run list [--workflow id] [--branch b] --limit N --json …
func ListRunsWith(ctx context.Context, run Runner, repoDir string, opts RunListOpts) ([]RunInfo, error) {
	if run == nil {
		run = defaultRunner
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultRunListLimit
	}
	args := []string{"run", "list",
		"--limit", strconv.Itoa(limit),
		"--json", "databaseId,number,attempt,displayTitle,workflowName,workflowDatabaseId,headBranch,headSha,event,status,conclusion,url,createdAt,updatedAt",
	}
	if opts.WorkflowID > 0 {
		args = append(args, "--workflow", strconv.FormatInt(opts.WorkflowID, 10))
	}
	if b := strings.TrimSpace(opts.Branch); b != "" {
		args = append(args, "--branch", b)
	}
	raw, err := run(ctx, repoDir, "gh", args...)
	if err != nil {
		return nil, err
	}
	return ParseRunsJSON(raw)
}

// GetRunDetail loads one run with jobs and steps.
func GetRunDetail(ctx context.Context, repoDir string, runID int64) (RunDetail, error) {
	return RunDetailWith(ctx, defaultRunner, repoDir, runID)
}

// RunDetailWith is GetRunDetail with an injectable runner.
// Equivalent to: gh run view <id> --json jobs,status,conclusion,…
func RunDetailWith(ctx context.Context, run Runner, repoDir string, runID int64) (RunDetail, error) {
	if run == nil {
		run = defaultRunner
	}
	if runID <= 0 {
		return RunDetail{}, fmt.Errorf("invalid run id")
	}
	raw, err := run(ctx, repoDir, "gh", "run", "view", strconv.FormatInt(runID, 10),
		"--json", "jobs,status,conclusion,displayTitle,workflowName,headBranch,headSha,event,createdAt,updatedAt,url,attempt")
	if err != nil {
		return RunDetail{}, err
	}
	detail, err := ParseRunDetailJSON(raw)
	if err != nil {
		return RunDetail{}, err
	}
	detail.ID = runID
	return detail, nil
}

// JobLog fetches the log for one job of a run, tail-capped to maxRunes.
// gh only serves logs for completed jobs.
func JobLog(ctx context.Context, repoDir string, runID, jobID int64, maxRunes int) (string, error) {
	return JobLogWith(ctx, defaultRunner, repoDir, runID, jobID, maxRunes)
}

// JobLogWith is JobLog with an injectable runner.
// Equivalent to: gh run view <runID> --job <jobID> --log
func JobLogWith(ctx context.Context, run Runner, repoDir string, runID, jobID int64, maxRunes int) (string, error) {
	if run == nil {
		run = defaultRunner
	}
	if runID <= 0 {
		return "", fmt.Errorf("invalid run id")
	}
	if jobID <= 0 {
		return "", fmt.Errorf("invalid job id")
	}
	if maxRunes <= 0 {
		maxRunes = DefaultJobLogRunes
	}
	raw, err := run(ctx, repoDir, "gh", "run", "view", strconv.FormatInt(runID, 10),
		"--job", strconv.FormatInt(jobID, 10), "--log")
	if err != nil {
		return "", err
	}
	return tailRunes(string(raw), maxRunes), nil
}

// DispatchWorkflow triggers a workflow_dispatch run.
// Dispatch is async: gh prints nothing useful on success and returns no run id.
func DispatchWorkflow(ctx context.Context, repoDir, owner, repo, workflowPath, ref string, inputs map[string]string) error {
	return DispatchWorkflowWith(ctx, defaultRunner, repoDir, owner, repo, workflowPath, ref, inputs)
}

// DispatchWorkflowWith is DispatchWorkflow with an injectable runner.
// Equivalent to: gh workflow run <basename> --ref <ref> [-f k=v …] [--repo owner/repo]
func DispatchWorkflowWith(ctx context.Context, run Runner, repoDir, owner, repo, workflowPath, ref string, inputs map[string]string) error {
	if run == nil {
		run = defaultRunner
	}
	workflow := filepath.Base(strings.TrimSpace(workflowPath))
	if workflow == "" || workflow == "." || workflow == string(filepath.Separator) {
		return fmt.Errorf("empty workflow path")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("empty ref")
	}
	args := []string{"workflow", "run", workflow, "--ref", ref}
	if len(inputs) > 0 {
		names := make([]string, 0, len(inputs))
		for k := range inputs {
			names = append(names, k)
		}
		slices.Sort(names)
		for _, k := range names {
			args = append(args, "-f", k+"="+inputs[k])
		}
	}
	if o, r := strings.TrimSpace(owner), strings.TrimSpace(repo); o != "" && r != "" {
		args = append(args, "--repo", o+"/"+r)
	}
	if _, err := run(ctx, repoDir, "gh", args...); err != nil {
		return err
	}
	return nil
}

// ListRemoteBranches lists origin remote-tracking branch names (no "origin/" prefix).
// Primary (main, else master) is first when present; the rest are sorted.
func ListRemoteBranches(ctx context.Context, repoDir string) ([]string, error) {
	return ListRemoteBranchesWith(ctx, defaultRunner, repoDir)
}

// ListRemoteBranchesWith is ListRemoteBranches with an injectable runner.
// Equivalent to: git for-each-ref refs/remotes/origin --format=%(refname:strip=3)
func ListRemoteBranchesWith(ctx context.Context, run Runner, repoDir string) ([]string, error) {
	if run == nil {
		run = defaultRunner
	}
	raw, err := run(ctx, repoDir, "git", "for-each-ref", "refs/remotes/origin",
		"--format=%(refname:strip=3)")
	if err != nil {
		return nil, err
	}
	return parseRemoteBranches(raw), nil
}

// RunBucket maps a GitHub Actions status/conclusion pair onto the same badge
// buckets used by checks: pass, fail, pending, skipping, cancel, other.
func RunBucket(status, conclusion string) string {
	st := strings.ToLower(strings.TrimSpace(status))
	conc := strings.ToLower(strings.TrimSpace(conclusion))
	// Not finished yet — treat as pending regardless of a stale conclusion.
	switch st {
	case "queued", "in_progress", "waiting", "requested", "pending", "waiting_for_progress":
		return "pending"
	}
	switch conc {
	case "success":
		return "pass"
	case "failure", "timed_out", "action_required", "startup_failure", "error":
		return "fail"
	case "cancelled", "canceled":
		return "cancel"
	case "skipped", "neutral", "stale":
		return "skipping"
	case "":
		if st == "completed" {
			return "other"
		}
		if st == "" {
			return "other"
		}
		return "pending"
	default:
		return "other"
	}
}

// ParseWorkflowsJSON parses gh workflow list --json output.
func ParseWorkflowsJSON(raw []byte) ([]WorkflowInfo, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "[]" {
		return nil, nil
	}
	var rows []workflowRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("gh workflow list json: %w", err)
	}
	out := make([]WorkflowInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, WorkflowInfo{
			ID:    r.ID,
			Name:  r.Name,
			Path:  r.Path,
			State: r.State,
		})
	}
	return out, nil
}

// ParseRunsJSON parses gh run list --json output.
func ParseRunsJSON(raw []byte) ([]RunInfo, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "[]" {
		return nil, nil
	}
	var rows []runListRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("gh run list json: %w", err)
	}
	out := make([]RunInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, RunInfo{
			ID:           r.DatabaseID,
			Number:       r.Number,
			Attempt:      r.Attempt,
			Title:        r.DisplayTitle,
			WorkflowName: r.WorkflowName,
			WorkflowID:   r.WorkflowDatabaseID,
			Branch:       r.HeadBranch,
			HeadSHA:      r.HeadSHA,
			Event:        r.Event,
			Status:       r.Status,
			Conclusion:   r.Conclusion,
			URL:          r.URL,
			CreatedAt:    parseGHTime(r.CreatedAt),
			UpdatedAt:    parseGHTime(r.UpdatedAt),
		})
	}
	return out, nil
}

// ParseRunDetailJSON parses gh run view --json output (jobs + metadata).
func ParseRunDetailJSON(raw []byte) (RunDetail, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return RunDetail{}, fmt.Errorf("gh run view json: empty")
	}
	var row runDetailRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return RunDetail{}, fmt.Errorf("gh run view json: %w", err)
	}
	detail := RunDetail{
		Attempt:      row.Attempt,
		Title:        row.DisplayTitle,
		WorkflowName: row.WorkflowName,
		Branch:       row.HeadBranch,
		HeadSHA:      row.HeadSHA,
		Event:        row.Event,
		Status:       row.Status,
		Conclusion:   row.Conclusion,
		URL:          row.URL,
		CreatedAt:    parseGHTime(row.CreatedAt),
		UpdatedAt:    parseGHTime(row.UpdatedAt),
	}
	if len(row.Jobs) > 0 {
		detail.Jobs = make([]RunJob, 0, len(row.Jobs))
		for _, j := range row.Jobs {
			job := RunJob{
				ID:          j.DatabaseID,
				Name:        j.Name,
				Status:      j.Status,
				Conclusion:  j.Conclusion,
				URL:         j.URL,
				StartedAt:   parseGHTime(j.StartedAt),
				CompletedAt: parseGHTime(j.CompletedAt),
			}
			if len(j.Steps) > 0 {
				job.Steps = make([]RunStep, 0, len(j.Steps))
				for _, s := range j.Steps {
					job.Steps = append(job.Steps, RunStep{
						Name:       s.Name,
						Status:     s.Status,
						Conclusion: s.Conclusion,
						Number:     s.Number,
					})
				}
			}
			detail.Jobs = append(detail.Jobs, job)
		}
	}
	return detail, nil
}

type workflowRow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type runListRow struct {
	DatabaseID         int64  `json:"databaseId"`
	Number             int    `json:"number"`
	Attempt            int    `json:"attempt"`
	DisplayTitle       string `json:"displayTitle"`
	WorkflowName       string `json:"workflowName"`
	WorkflowDatabaseID int64  `json:"workflowDatabaseId"`
	HeadBranch         string `json:"headBranch"`
	HeadSHA            string `json:"headSha"`
	Event              string `json:"event"`
	Status             string `json:"status"`
	Conclusion         string `json:"conclusion"`
	URL                string `json:"url"`
	CreatedAt          string `json:"createdAt"`
	UpdatedAt          string `json:"updatedAt"`
}

type runDetailRow struct {
	Attempt      int         `json:"attempt"`
	DisplayTitle string      `json:"displayTitle"`
	WorkflowName string      `json:"workflowName"`
	HeadBranch   string      `json:"headBranch"`
	HeadSHA      string      `json:"headSha"`
	Event        string      `json:"event"`
	Status       string      `json:"status"`
	Conclusion   string      `json:"conclusion"`
	URL          string      `json:"url"`
	CreatedAt    string      `json:"createdAt"`
	UpdatedAt    string      `json:"updatedAt"`
	Jobs         []runJobRow `json:"jobs"`
}

type runJobRow struct {
	DatabaseID  int64        `json:"databaseId"`
	Name        string       `json:"name"`
	Status      string       `json:"status"`
	Conclusion  string       `json:"conclusion"`
	URL         string       `json:"url"`
	StartedAt   string       `json:"startedAt"`
	CompletedAt string       `json:"completedAt"`
	Steps       []runStepRow `json:"steps"`
}

type runStepRow struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

func parseRemoteBranches(raw []byte) []string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	var branches []string
	seen := map[string]struct{}{}
	for line := range strings.SplitSeq(text, "\n") {
		b := strings.TrimSpace(line)
		if b == "" || b == "HEAD" {
			continue
		}
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		branches = append(branches, b)
	}
	return sortBranchesPrimaryFirst(branches)
}

func sortBranchesPrimaryFirst(branches []string) []string {
	if len(branches) == 0 {
		return nil
	}
	primary := ""
	for _, cand := range []string{"main", "master"} {
		if slices.Contains(branches, cand) {
			primary = cand
			break
		}
	}
	rest := make([]string, 0, len(branches))
	for _, b := range branches {
		if b != primary {
			rest = append(rest, b)
		}
	}
	slices.Sort(rest)
	if primary == "" {
		return rest
	}
	return append([]string{primary}, rest...)
}
