package bot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPathGlobRegexp(t *testing.T) {
	tests := []struct {
		glob, path string
		want       bool
	}{
		{"**/migrations/**", "db/migrations/001.sql", true},
		{"**/migrations/**", "migrations/001.sql", true},
		{"**/migrations/**", "db/migrate/001.sql", false},
		{"**/*migration*", "pkg/usermigration.go", true},
		{"**/auth/**", "internal/auth/login.go", true},
		{"**/.env", ".env", true},
		{"**/.env.*", "config/.env.local", true},
		{"**/Dockerfile*", "deploy/Dockerfile.prod", true},
		{"**/*.tf", "infra/main.tf", true},
		{"**/auth/**", "internal/other/login.go", false},
	}
	for _, tt := range tests {
		re, err := pathGlobRegexp(tt.glob)
		if err != nil {
			t.Fatalf("glob %q: %v", tt.glob, err)
		}
		if got := re.MatchString(tt.path); got != tt.want {
			t.Errorf("glob %q path %q: got %v want %v", tt.glob, tt.path, got, tt.want)
		}
	}
}

func TestFilterRiskyPaths(t *testing.T) {
	paths := []string{
		"api/handler.go",
		"db/migrations/002_add.sql",
		"internal/auth/token.go",
		"README.md",
	}
	got := filterRiskyPaths(paths, DefaultRiskyPathGlobs)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestParseDiffStatSummary(t *testing.T) {
	stat := " foo.go | 10 ++++\n bar.go | 2 +\n 2 files changed, 12 insertions(+), 0 deletions(-)"
	ins, del, files := parseDiffStatSummary(stat)
	if files != 2 || ins != 12 || del != 0 {
		t.Fatalf("files=%d ins=%d del=%d", files, ins, del)
	}
	ins, del, files = parseDiffStatSummary("1 file changed, 1 insertion(+), 3 deletions(-)")
	if files != 1 || ins != 1 || del != 3 {
		t.Fatalf("single: files=%d ins=%d del=%d", files, ins, del)
	}
}

func TestFormatCompletionCard(t *testing.T) {
	card := FormatCompletionCard(CompletionCardInput{
		Status:   "Done",
		Project:  "api",
		Elapsed:  2*time.Minute + 5*time.Second,
		Branch:   "grok/discord/1",
		PRURL:    "https://github.com/o/r/pull/9",
		PRNumber: 9,
		Diff: DiffSummary{
			Branch:     "grok/discord/1",
			HeadShort:  "abc1234",
			BaseRef:    "origin/main",
			FileCount:  2,
			Insertions: 10,
			Deletions:  1,
			HasCommits: true,
			NameStatus: []string{"M\tapi/a.go", "A\tdb/migrations/1.sql"},
			Risky:      []string{"db/migrations/1.sql"},
		},
	})
	for _, want := range []string{
		"**Summary**", "Done", "api", "2m",
		"grok/discord/1", "abc1234", "origin/main",
		"+10", "-1", "M api/a.go", "risk", "migrations",
		"#9", "https://github.com/o/r/pull/9",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("missing %q in:\n%s", want, card)
		}
	}

	empty := FormatCompletionCard(CompletionCardInput{
		Status:  "Done",
		Project: "api",
		Diff:    DiffSummary{},
	})
	if empty != "" {
		t.Fatalf("expected empty card, got %q", empty)
	}
}

func TestCollectDiffSummary(t *testing.T) {
	repo := initCompletionTestRepo(t)
	ctx := context.Background()

	// On main with no extra commits: empty-ish.
	sum, err := CollectDiffSummary(ctx, repo, DefaultRiskyPathGlobs)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasCommits {
		t.Fatalf("main should not be ahead: %+v", sum)
	}

	// Feature branch with commit + risky file.
	runGit(t, repo, "checkout", "-b", "grok/discord/test1")
	mig := filepath.Join(repo, "db", "migrations")
	if err := os.MkdirAll(mig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mig, "001.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "add migration")

	sum, err = CollectDiffSummary(ctx, repo, DefaultRiskyPathGlobs)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasCommits {
		t.Fatal("expected commits ahead of base")
	}
	if sum.FileCount < 1 {
		t.Fatalf("file count: %+v", sum)
	}
	if len(sum.Risky) == 0 {
		t.Fatalf("expected risky migration path, names=%v risk=%v", sum.NameStatus, sum.Risky)
	}
	found := false
	for _, r := range sum.Risky {
		if strings.Contains(r, "migrations") {
			found = true
		}
	}
	if !found {
		t.Fatalf("risky=%v", sum.Risky)
	}

	card := FormatCompletionCard(CompletionCardInput{
		Status:  "Done",
		Project: "app",
		Elapsed: time.Minute,
		Branch:  sum.Branch,
		Diff:    sum,
	})
	if card == "" || !strings.Contains(card, "risk") {
		t.Fatalf("card=%q", card)
	}
}

func initCompletionTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	// Default branch main.
	runGit(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
