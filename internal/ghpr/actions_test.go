package ghpr

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestListWorkflowsWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("name=%s", name)
		}
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "workflow list --all") {
			t.Fatalf("args=%v", args)
		}
		if !strings.Contains(joined, "--json id,name,path,state") {
			t.Fatalf("json fields: %v", args)
		}
		return []byte(`[
			{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active"},
			{"id":2,"name":"Deploy","path":".github/workflows/deploy.yml","state":"disabled_manually"}
		]`), nil
	}
	list, err := ListWorkflowsWith(t.Context(), run, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].ID != 1 || list[0].Name != "CI" || list[0].Path != ".github/workflows/ci.yml" {
		t.Fatalf("first=%+v", list[0])
	}
	if list[1].State != "disabled_manually" {
		t.Fatalf("second=%+v", list[1])
	}
}

func TestListRunsWithMock(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return []byte(`[
			{
				"databaseId":99,"number":7,"attempt":1,
				"displayTitle":"fix: timeout","workflowName":"CI","workflowDatabaseId":1,
				"headBranch":"main","headSha":"abc1234","event":"push",
				"status":"completed","conclusion":"success",
				"url":"https://github.com/o/r/actions/runs/99",
				"createdAt":"2026-07-01T10:00:00Z","updatedAt":"2026-07-01T10:05:00Z"
			}
		]`), nil
	}
	list, err := ListRunsWith(t.Context(), run, "/repo", RunListOpts{
		WorkflowID: 1,
		Branch:     "main",
		Limit:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "run list") {
		t.Fatalf("args=%v", saw)
	}
	if !strings.Contains(joined, "--workflow 1") || !strings.Contains(joined, "--branch main") {
		t.Fatalf("filters: %v", saw)
	}
	if !strings.Contains(joined, "--limit 5") {
		t.Fatalf("limit: %v", saw)
	}
	for _, field := range []string{
		"databaseId", "displayTitle", "workflowDatabaseId", "headBranch", "headSha",
		"createdAt", "updatedAt",
	} {
		if !strings.Contains(joined, field) {
			t.Fatalf("missing json field %q in %v", field, saw)
		}
	}
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	r := list[0]
	if r.ID != 99 || r.Number != 7 || r.Title != "fix: timeout" || r.WorkflowID != 1 {
		t.Fatalf("%+v", r)
	}
	if r.Branch != "main" || r.HeadSHA != "abc1234" || r.Event != "push" {
		t.Fatalf("%+v", r)
	}
	if r.CreatedAt.IsZero() || r.CreatedAt.UTC().Format("2006-01-02 15:04") != "2026-07-01 10:00" {
		t.Fatalf("createdAt=%v", r.CreatedAt)
	}
}

func TestListRunsDefaultLimit(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = args
		return []byte(`[]`), nil
	}
	if _, err := ListRunsWith(t.Context(), run, "/repo", RunListOpts{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "--limit 20") {
		t.Fatalf("default limit: %v", saw)
	}
	if strings.Contains(joined, "--workflow") || strings.Contains(joined, "--branch") {
		t.Fatalf("unexpected filters: %v", saw)
	}
}

func TestRunDetailWithMock(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return []byte(`{
			"attempt":2,"displayTitle":"deploy","workflowName":"Deploy",
			"headBranch":"production","headSha":"deadbeef","event":"workflow_dispatch",
			"status":"completed","conclusion":"failure",
			"url":"https://github.com/o/r/actions/runs/50",
			"createdAt":"2026-07-02T12:00:00Z","updatedAt":"2026-07-02T12:10:00Z",
			"jobs":[{
				"databaseId":501,"name":"build","status":"completed","conclusion":"failure",
				"url":"https://github.com/o/r/actions/runs/50/job/501",
				"startedAt":"2026-07-02T12:00:01Z","completedAt":"2026-07-02T12:09:00Z",
				"steps":[
					{"name":"Checkout","status":"completed","conclusion":"success","number":1},
					{"name":"Build","status":"completed","conclusion":"failure","number":2}
				]
			}]
		}`), nil
	}
	detail, err := RunDetailWith(t.Context(), run, "/repo", 50)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "run view 50") || !strings.Contains(joined, "--json") {
		t.Fatalf("args=%v", saw)
	}
	if !strings.Contains(joined, "jobs") || !strings.Contains(joined, "displayTitle") {
		t.Fatalf("json fields: %v", saw)
	}
	if detail.ID != 50 || detail.Attempt != 2 || detail.Title != "deploy" {
		t.Fatalf("%+v", detail)
	}
	if detail.Branch != "production" || detail.Conclusion != "failure" {
		t.Fatalf("%+v", detail)
	}
	if len(detail.Jobs) != 1 || detail.Jobs[0].ID != 501 || detail.Jobs[0].Name != "build" {
		t.Fatalf("jobs=%+v", detail.Jobs)
	}
	if len(detail.Jobs[0].Steps) != 2 || detail.Jobs[0].Steps[1].Conclusion != "failure" {
		t.Fatalf("steps=%+v", detail.Jobs[0].Steps)
	}
}

func TestRunDetailInvalidID(t *testing.T) {
	_, err := RunDetailWith(t.Context(), func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}, "/repo", 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestJobLogWithMock(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return []byte("line1\nline2\nline3\n"), nil
	}
	log, err := JobLogWith(t.Context(), run, "/repo", 50, 501, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "run view 50") || !strings.Contains(joined, "--job 501") || !strings.Contains(joined, "--log") {
		t.Fatalf("args=%v", saw)
	}
	if !strings.Contains(log, "line3") {
		t.Fatalf("log=%q", log)
	}
}

func TestJobLogTailCap(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("abcdefghijklmnopqrstuvwxyz"), nil
	}
	log, err := JobLogWith(t.Context(), run, "/repo", 1, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	// tailRunes prefixes with "…\n" when truncated.
	if !strings.HasPrefix(log, "…\n") {
		t.Fatalf("expected truncated prefix: %q", log)
	}
	if !strings.HasSuffix(log, "vwxyz") {
		t.Fatalf("tail=%q", log)
	}
}

func TestDispatchWorkflowWithArgs(t *testing.T) {
	var saw []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		saw = append([]string{name}, args...)
		return nil, nil
	}
	err := DispatchWorkflowWith(t.Context(), run, "/repo", "acme", "app",
		".github/workflows/deploy.yml", "production",
		map[string]string{"env": "prod", "dry_run": "true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(saw, " ")
	if !strings.Contains(joined, "workflow run deploy.yml") {
		t.Fatalf("basename: %v", saw)
	}
	if !strings.Contains(joined, "--ref production") {
		t.Fatalf("ref: %v", saw)
	}
	if !strings.Contains(joined, "--repo acme/app") {
		t.Fatalf("repo: %v", saw)
	}
	// -f pairs sorted by name: dry_run before env
	dryIdx := indexOfArg(saw, "dry_run=true")
	envIdx := indexOfArg(saw, "env=prod")
	if dryIdx < 0 || envIdx < 0 || dryIdx > envIdx {
		t.Fatalf("sorted -f: %v", saw)
	}
	if saw[dryIdx-1] != "-f" || saw[envIdx-1] != "-f" {
		t.Fatalf("-f flags: %v", saw)
	}
}

func TestDispatchWorkflowOmitsRepoWhenPartial(t *testing.T) {
	cases := []struct {
		owner, repo string
	}{
		{"", ""},
		{"acme", ""},
		{"", "app"},
	}
	for _, tc := range cases {
		var saw []string
		run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			saw = args
			return nil, nil
		}
		if err := DispatchWorkflowWith(t.Context(), run, "/repo", tc.owner, tc.repo, "ci.yml", "main", nil); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(saw, " ")
		if strings.Contains(joined, "--repo") {
			t.Fatalf("owner=%q repo=%q should omit --repo: %v", tc.owner, tc.repo, saw)
		}
	}
}

func TestDispatchWorkflowValidation(t *testing.T) {
	noop := func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}
	if err := DispatchWorkflowWith(t.Context(), noop, "/r", "o", "r", "", "main", nil); err == nil {
		t.Fatal("empty workflow")
	}
	if err := DispatchWorkflowWith(t.Context(), noop, "/r", "o", "r", "ci.yml", "", nil); err == nil {
		t.Fatal("empty ref")
	}
}

func TestListRemoteBranchesWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("name=%s", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "for-each-ref") || !strings.Contains(joined, "refs/remotes/origin") {
			t.Fatalf("args=%v", args)
		}
		return []byte("HEAD\nfeature/x\nmaster\nzed\nmain\n"), nil
	}
	list, err := ListRemoteBranchesWith(t.Context(), run, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0] != "main" {
		t.Fatalf("primary first: %v", list)
	}
	if strings.Contains(strings.Join(list, ","), "HEAD") {
		t.Fatalf("HEAD leaked: %v", list)
	}
	// rest sorted: feature/x, master, zed
	want := []string{"main", "feature/x", "master", "zed"}
	if strings.Join(list, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", list, want)
	}
}

func TestListRemoteBranchesMasterPrimary(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("develop\nmaster\n"), nil
	}
	list, err := ListRemoteBranchesWith(t.Context(), run, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if list[0] != "master" {
		t.Fatalf("%v", list)
	}
}

func TestRunBucket(t *testing.T) {
	cases := []struct {
		status, conclusion, want string
	}{
		{"completed", "success", "pass"},
		{"completed", "failure", "fail"},
		{"completed", "timed_out", "fail"},
		{"completed", "cancelled", "cancel"},
		{"completed", "skipped", "skipping"},
		{"in_progress", "", "pending"},
		{"queued", "", "pending"},
		{"in_progress", "success", "pending"}, // in-flight wins
		{"", "", "other"},
	}
	for _, tc := range cases {
		got := RunBucket(tc.status, tc.conclusion)
		if got != tc.want {
			t.Errorf("RunBucket(%q,%q)=%q want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

func TestParseWorkflowsJSONEmpty(t *testing.T) {
	list, err := ParseWorkflowsJSON([]byte("[]"))
	if err != nil || list != nil {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestParseRunsJSONBad(t *testing.T) {
	_, err := ParseRunsJSON([]byte(`{`))
	if err == nil || !strings.Contains(err.Error(), "gh run list json") {
		t.Fatalf("err=%v", err)
	}
}

func TestWorkflowFileAtPrimaryWith(t *testing.T) {
	var calls []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "rev-parse --verify refs/remotes/origin/HEAD") {
			return []byte("abcdeadbeef\n"), nil
		}
		if strings.HasPrefix(joined, "cat-file blob abcdeadbeef:.github/workflows/ci.yml") {
			return []byte("name: CI\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	raw, err := WorkflowFileAtPrimaryWith(t.Context(), run, "/repo", ".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "name: CI\n" {
		t.Fatalf("raw=%q", raw)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestWorkflowFileAtPrimaryFallback(t *testing.T) {
	var tried []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "rev-parse") {
			tried = append(tried, args[len(args)-1])
			if strings.HasSuffix(joined, "origin/main") {
				return []byte("sha-main\n"), nil
			}
			return nil, fmt.Errorf("missing")
		}
		if strings.Contains(joined, "cat-file blob sha-main:") {
			return []byte("ok"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	raw, err := WorkflowFileAtPrimaryWith(t.Context(), run, "/repo", "wf.yml")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ok" {
		t.Fatalf("%q", raw)
	}
	if len(tried) < 2 || tried[0] != "refs/remotes/origin/HEAD" {
		t.Fatalf("tried=%v", tried)
	}
}

func TestWorkflowFileAtPrimaryRejectsEscape(t *testing.T) {
	_, err := WorkflowFileAtPrimaryWith(t.Context(), func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("should not run")
		return nil, nil
	}, "/repo", "../secret")
	if err == nil {
		t.Fatal("expected error")
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
