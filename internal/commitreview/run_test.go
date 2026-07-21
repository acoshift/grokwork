package commitreview

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/grokrun"
)

func TestExecuteHappyPath(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	full := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	git := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		joined := args[0]
		for _, a := range args[1:] {
			joined += " " + a
		}
		switch {
		case args[0] == "rev-parse":
			return []byte(full + "\n"), nil
		case args[0] == "show" && contains(args, "-s"):
			return []byte(full + "\x1fSubj\x1fA\x1fa@b\x1f2026-07-01T00:00:00Z\x1f\n"), nil
		case args[0] == "show" && contains(args, "--stat"):
			return []byte("f.go | 1 +\n"), nil
		case args[0] == "show" && contains(args, "-p"):
			return []byte("diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old\n+new\n"), nil
		default:
			t.Fatalf("git %v", args)
			return nil, nil
		}
	}
	created := 0
	create := func(ctx context.Context, repoDir, owner, repo string, opts ghpr.CreateIssueOpts) (int, string, error) {
		created++
		return 10 + created, "https://github.com/" + owner + "/" + repo + "/issues/1", nil
	}
	grok := func(ctx context.Context, opt grokrun.Options) grokrun.Result {
		if opt.Yolo {
			t.Fatalf("expected yolo-off: %+v", opt)
		}
		if opt.Tools == nil || *opt.Tools != ReviewTools {
			t.Fatalf("expected read-only tools %q, got %+v", ReviewTools, opt.Tools)
		}
		if strings.TrimSpace(opt.JSONSchema) == "" {
			t.Fatal("expected findings JSON schema")
		}
		if opt.MaxTurns != DefaultMaxTurns {
			t.Fatalf("expected default max turns %d, got %d", DefaultMaxTurns, opt.MaxTurns)
		}
		hasDenyMCP := false
		for i := 0; i+1 < len(opt.ExtraArgs); i++ {
			if opt.ExtraArgs[i] == "--deny" && opt.ExtraArgs[i+1] == "MCPTool" {
				hasDenyMCP = true
			}
		}
		if !hasDenyMCP {
			t.Fatalf("expected --deny MCPTool in ExtraArgs: %v", opt.ExtraArgs)
		}
		return grokrun.Result{Code: 0, Text: `{"summary":"risky","findings":[
			{"title":"Bug one","body":"details","severity":"high","paths":["f.go"]}
		]}`}
	}
	job := NewQueuedJob(StartOpts{
		Project: "p", Owner: "o", Repo: "r", SHA: full, Actor: "user",
	})
	if err := store.Save(job); err != nil {
		t.Fatal(err)
	}
	Execute(context.Background(), Deps{
		Store: store, Grok: grok, Create: create, Git: git,
		Timeout: 30 * time.Second,
	}, job, filepath.Join(dir, "repo"))

	got, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
	if got.Summary != "risky" || len(got.Findings) != 1 {
		t.Fatalf("%+v", got)
	}
	if got.Findings[0].IssueNumber == 0 || got.Findings[0].CreateError != "" {
		t.Fatalf("finding %+v", got.Findings[0])
	}
	if created != 1 {
		t.Fatalf("created=%d", created)
	}
}

func TestExecuteParseFail(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	full := "cccccccccccccccccccccccccccccccccccccccc"
	git := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		if args[0] == "rev-parse" {
			return []byte(full + "\n"), nil
		}
		if args[0] == "show" && contains(args, "-s") {
			return []byte(full + "\x1fS\x1fA\x1fa@b\x1f2026-07-01T00:00:00Z\x1f\n"), nil
		}
		if args[0] == "show" {
			return []byte(""), nil
		}
		return nil, nil
	}
	job := NewQueuedJob(StartOpts{Project: "p", Owner: "o", Repo: "r", SHA: full})
	_ = store.Save(job)
	Execute(context.Background(), Deps{
		Store: store,
		Git:   git,
		Grok:  func(ctx context.Context, opt grokrun.Options) grokrun.Result { return grokrun.Result{Code: 0, Text: "not json"} },
		Create: func(ctx context.Context, repoDir, owner, repo string, opts ghpr.CreateIssueOpts) (int, string, error) {
			t.Fatal("should not create")
			return 0, "", nil
		},
	}, job, dir)
	got, _ := store.Get(job.ID)
	if got.Status != StatusFailed {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.Error, "parse findings") || !strings.Contains(got.Error, "model said") {
		t.Fatalf("error should include parse failure + model text: %q", got.Error)
	}
}

func TestExecuteMaxTurnsParseFail(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	full := "dddddddddddddddddddddddddddddddddddddddd"
	git := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		if args[0] == "rev-parse" {
			return []byte(full + "\n"), nil
		}
		if args[0] == "show" && contains(args, "-s") {
			return []byte(full + "\x1fS\x1fA\x1fa@b\x1f2026-07-01T00:00:00Z\x1f\n"), nil
		}
		if args[0] == "show" {
			return []byte(""), nil
		}
		return nil, nil
	}
	job := NewQueuedJob(StartOpts{Project: "p", Owner: "o", Repo: "r", SHA: full})
	_ = store.Save(job)
	Execute(context.Background(), Deps{
		Store: store,
		Git:   git,
		Grok: func(ctx context.Context, opt grokrun.Options) grokrun.Result {
			return grokrun.Result{
				Code:            1,
				Text:            "Looking at related files next…",
				Stderr:          "Error: max turns reached\n",
				MaxTurnsReached: true,
			}
		},
		Create: func(ctx context.Context, repoDir, owner, repo string, opts ghpr.CreateIssueOpts) (int, string, error) {
			t.Fatal("should not create")
			return 0, "", nil
		},
	}, job, dir)
	got, _ := store.Get(job.ID)
	if got.Status != StatusFailed {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.Error, "max turns") {
		t.Fatalf("want max turns error, got %q", got.Error)
	}
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
