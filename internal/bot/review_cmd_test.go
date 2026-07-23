package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestParseReviewArgs(t *testing.T) {
	id, rest := parseReviewArgs("/review <@123456> please focus on auth")
	if id != "123456" {
		t.Fatalf("id=%q", id)
	}
	if rest != "please focus on auth" {
		t.Fatalf("rest=%q", rest)
	}
	id, rest = parseReviewArgs("/review <@!99> #42 fix tests")
	if id != "99" || rest != "#42 fix tests" {
		t.Fatalf("id=%q rest=%q", id, rest)
	}
	id, _ = parseReviewArgs("/review nobody")
	if id != "" {
		t.Fatal("expected empty without mention")
	}
}

func TestIsReviewCommand(t *testing.T) {
	if !isReviewCommand("/review @x") {
		t.Fatal("want true")
	}
	if isReviewCommand("review the flaky test") {
		t.Fatal("bare review must stay a task")
	}
}

func TestParseMessageReview(t *testing.T) {
	p := ParseMessage("<@BOT> /review <@111>", "BOT")
	if p.Kind != KindReview {
		t.Fatalf("got %v", p.Kind)
	}
	// Free-form without slash stays task.
	p = ParseMessage("<@BOT> review the flaky CI", "BOT")
	if p.Kind != KindTask {
		t.Fatalf("got %v want task", p.Kind)
	}
}

func TestResolveMappedGitHubLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["u1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path

	if got := ResolveMappedGitHubLogin(nil, "1"); got != "" {
		t.Fatalf("nil cfg: %q", got)
	}
	if got := ResolveMappedGitHubLogin(&cfg, "missing"); got != "" {
		t.Fatalf("empty map: %q", got)
	}
	if err := cfg.SetGitHubIdentity("42", config.GitHubIdentity{Login: "@alice-gh", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if got := ResolveMappedGitHubLogin(&cfg, "42"); got != "alice-gh" {
		t.Fatalf("mapped: %q", got)
	}
	if got := ResolveMappedGitHubLogin(&cfg, "99"); got != "" {
		t.Fatalf("unmapped other user: %q", got)
	}
	// Empty / whitespace-only login is rejected by Set; lookup of missing stays empty.
	if err := cfg.SetGitHubIdentity("empty", config.GitHubIdentity{Login: "  "}); err == nil {
		t.Fatal("expected empty login rejected")
	}
	if got := ResolveMappedGitHubLogin(&cfg, "empty"); got != "" {
		t.Fatalf("empty login must stay unmapped: %q", got)
	}
}

func TestRequestFormalGitHubReviewMapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["rev1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	if err := cfg.SetGitHubIdentity("rev1", config.GitHubIdentity{Login: "bob-gh"}); err != nil {
		t.Fatal(err)
	}

	var calls []string
	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("ok"), nil
	}
	login, err := requestFormalGitHubReview(context.Background(), run, &cfg, dir, "acme", "app", 9, "rev1")
	if err != nil {
		t.Fatal(err)
	}
	if login != "bob-gh" {
		t.Fatalf("login=%q", login)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
	if !strings.Contains(calls[0], "pr edit 9") {
		t.Fatalf("want pr edit: %s", calls[0])
	}
	if !strings.Contains(calls[0], "--add-reviewer bob-gh") {
		t.Fatalf("want mapped reviewer: %s", calls[0])
	}
	if !strings.Contains(calls[0], "--repo acme/app") {
		t.Fatalf("want repo: %s", calls[0])
	}
}

func TestRequestFormalGitHubReviewUnmappedNoGH(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["u1"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path

	calls := 0
	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("should not run")
	}
	login, err := requestFormalGitHubReview(context.Background(), run, &cfg, dir, "acme", "app", 9, "nobody")
	if err != nil {
		t.Fatalf("unmapped should not error: %v", err)
	}
	if login != "" {
		t.Fatalf("login=%q", login)
	}
	if calls != 0 {
		t.Fatalf("gh must not run for unmapped, calls=%d", calls)
	}
}

func TestRequestFormalGitHubReviewGHError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "discordToken": "tok",
  "projects": {"app": {"path": "`+filepath.ToSlash(dir)+`", "allowedUserIds": ["r"]}},
  "channels": {},
  "grokBin": "grok"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg config.Config
	_ = json.Unmarshal(raw, &cfg)
	cfg.ConfigPath = path
	_ = cfg.SetGitHubIdentity("r", config.GitHubIdentity{Login: "x"})

	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh: user not found")
	}
	login, err := requestFormalGitHubReview(context.Background(), run, &cfg, dir, "o", "r", 1, "r")
	if login != "x" {
		t.Fatalf("login=%q", login)
	}
	if err == nil || !strings.Contains(err.Error(), "user not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestFormatReviewRequestReply(t *testing.T) {
	mappedOK := formatReviewRequestReply(reviewRequestReply{
		ReviewerID: "1", RequesterID: "2", Owner: "o", Repo: "r", Number: 3,
		TeamOK: true, GitHubLogin: "alice",
	})
	if !strings.Contains(mappedOK, "Also requested formal GitHub review from @alice") {
		t.Fatalf("mapped ok:\n%s", mappedOK)
	}
	if strings.Contains(mappedOK, "not a formal GitHub") {
		t.Fatalf("must not claim unmapped:\n%s", mappedOK)
	}

	unmapped := formatReviewRequestReply(reviewRequestReply{
		ReviewerID: "1", RequesterID: "2", Owner: "o", Repo: "r", Number: 3,
		TeamOK: true,
	})
	if !strings.Contains(unmapped, "team request only") {
		t.Fatalf("unmapped:\n%s", unmapped)
	}
	if strings.Contains(unmapped, "Also requested formal") {
		t.Fatalf("unmapped claimed formal:\n%s", unmapped)
	}

	ghFail := formatReviewRequestReply(reviewRequestReply{
		ReviewerID: "1", RequesterID: "2", Owner: "o", Repo: "r", Number: 3,
		TeamOK: true, GitHubLogin: "bob", GitHubErr: errors.New("denied"),
	})
	if !strings.Contains(ghFail, "Team request saved") || !strings.Contains(ghFail, "@bob") {
		t.Fatalf("gh fail:\n%s", ghFail)
	}
}

func TestReviewHelpMentionsGitHubMap(t *testing.T) {
	h := reviewHelpText()
	if !strings.Contains(h, "GitHub") {
		t.Fatalf("help should mention GitHub: %s", h)
	}
}
