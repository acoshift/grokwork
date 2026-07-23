package bot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParseWave2Commands(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
	}{
		{"/checkpoint before refactor", KindCheckpoint},
		{"/undo", KindUndo},
		{"/restore ab12", KindRestore},
		{"/verify unit", KindVerify},
		{"/sync", KindSync},
		{"/comments", KindComments},
		{"/address", KindAddress},
		{"please verify the fix carefully", KindTask}, // freeform
		{"address the nits please", KindTask},
	}
	for _, tc := range cases {
		p := ParseMessage("<@1> "+tc.in, "1")
		if p.Kind != tc.kind {
			t.Errorf("%q: kind=%s want %s", tc.in, kindName(p.Kind), kindName(tc.kind))
		}
	}
}

func TestParseDecisionBlocks(t *testing.T) {
	text := `Some analysis...

DECISION:
id: q1
prompt: Bump timeout to 30s?
options: Yes|No|Need more data

Done.`
	specs := parseDecisionBlocks(text)
	if len(specs) != 1 {
		t.Fatalf("specs=%d", len(specs))
	}
	if specs[0].ID != "q1" || specs[0].Prompt == "" || len(specs[0].Options) != 3 {
		t.Fatalf("%+v", specs[0])
	}
	tid, qid, idx, ok := parseDecisionCustomID("gd:d:123:q1:1")
	if !ok || tid != "123" || qid != "q1" || idx != 1 {
		t.Fatalf("custom id parse: %v %v %v %v", tid, qid, idx, ok)
	}
}

func TestPreserveWave2Fields(t *testing.T) {
	prev := sessionstore.Entry{
		Mode: "fix",
		Checkpoints: []sessionstore.CheckpointMeta{
			{ID: "c1", Ref: "refs/grok-cp/t/c1", SHA: "abc"},
		},
		OpenQuestions: []sessionstore.OpenQuestion{
			{ID: "q1", Text: "?", Status: "open"},
		},
		VerifyMsgID: "m1",
		LastVerify:  &sessionstore.LastVerify{Name: "unit", OK: true, Summary: "unit pass"},
	}
	next := sessionstore.Entry{SessionID: "s"}
	preservePRFields(&next, prev)
	if len(next.Checkpoints) != 1 || next.Checkpoints[0].ID != "c1" {
		t.Fatalf("checkpoints: %+v", next.Checkpoints)
	}
	if len(next.OpenQuestions) != 1 || next.VerifyMsgID != "m1" {
		t.Fatalf("wave2: %+v", next)
	}
	if next.LastVerify == nil || !next.LastVerify.OK || next.LastVerify.Name != "unit" {
		t.Fatalf("LastVerify: %+v", next.LastVerify)
	}
}

func TestLastVerifyFromResults(t *testing.T) {
	lv := lastVerifyFromResults([]verifyResult{
		{Name: "unit", OK: true, ExitCode: 0, Elapsed: time.Second, Log: "ok"},
		{Name: "lint", OK: false, ExitCode: 2, Elapsed: time.Millisecond * 50, Log: "err line"},
	})
	if lv == nil || lv.OK || lv.ExitCode != 2 {
		t.Fatalf("%+v", lv)
	}
	if !strings.Contains(lv.Name, "unit") || !strings.Contains(lv.Name, "lint") {
		t.Fatalf("names %q", lv.Name)
	}
	if lv.LogTail != "err line" {
		t.Fatalf("log %q", lv.LogTail)
	}
}

func TestCheckpointCreateRestoreK8(t *testing.T) {
	repo := t.TempDir()
	runGitWave2(t, repo, "init")
	runGitWave2(t, repo, "config", "user.email", "t@t")
	runGitWave2(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWave2(t, repo, "add", "f.txt")
	runGitWave2(t, repo, "commit", "-m", "one")
	runGitWave2(t, repo, "branch", "-M", "main")
	// Create managed branch worktree-style: just checkout a managed branch name in-repo.
	branch := "grokwork/thr-wave2"
	runGitWave2(t, repo, "checkout", "-b", branch)

	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.PathProjects(map[string]string{"app": repo}),
	}
	b := New(cfg, store, nil)
	threadID := "thr-wave2"
	if err := store.Set(threadID, sessionstore.Entry{
		Project:        "app",
		Cwd:            repo,
		WorktreeBranch: branch,
		OwnerID:        "u1",
	}); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get(threadID)
	meta, err := b.createCheckpoint(threadID, e, Actor{ID: "u1"}, "before")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.SHA == "" {
		t.Fatalf("%+v", meta)
	}
	// Second commit
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWave2(t, repo, "add", "f.txt")
	runGitWave2(t, repo, "commit", "-m", "two")

	e, _ = store.Get(threadID)
	cp, ok := e.FindCheckpoint(meta.ID)
	if !ok {
		t.Fatal("missing meta")
	}
	// Cross-thread ref must fail IsCheckpointRefForThread
	if gitworktree.IsCheckpointRefForThread(cp.Ref, "other-thread") {
		t.Fatal("cross-thread should fail")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gitworktree.HardResetToSHA(ctx, repo, cp.SHA); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(repo, "f.txt"))
	if string(body) != "one" {
		t.Fatalf("content=%q", body)
	}
}

func TestFilterVerifyCmds(t *testing.T) {
	cmds := []config.VerifyCommand{
		{Name: "unit", Command: "go test ./..."},
		{Name: "lint", Command: "golangci-lint run"},
	}
	if got := filterVerifyCmds(cmds, ""); len(got) != 2 {
		t.Fatalf("all: %d", len(got))
	}
	if got := filterVerifyCmds(cmds, "unit"); len(got) != 1 || got[0].Name != "unit" {
		t.Fatalf("%+v", got)
	}
	if got := filterVerifyCmds(cmds, "missing"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestVerifyCommandsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{
		Projects:   config.PathProjects(map[string]string{"app": dir}),
		ConfigPath: cfgPath,
	}
	if err := cfg.SetProjectVerifyCommands("app", []config.VerifyCommand{
		{Name: "unit", Command: "go test ./...", TimeoutMs: 60_000},
	}); err != nil {
		t.Fatal(err)
	}
	got := cfg.ProjectVerifyCommands("app")
	if len(got) != 1 || got[0].Name != "unit" {
		t.Fatalf("%+v", got)
	}
}

func TestHelpMentionsWave2(t *testing.T) {
	h := HelpText()
	for _, want := range []string{"/checkpoint", "/verify", "/sync", "/comments", "/address", "/undo"} {
		if !strings.Contains(h, want) {
			t.Errorf("help missing %s", want)
		}
	}
}

func runGitWave2(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
