package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/identity"
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

// linkedIdentity builds an identity store with one GitHub login attached to an
// account, which is the only way attribution is established now that the
// admin-maintained discordUserGitHub map is gone.
func linkedIdentity(t *testing.T, canonical, numericID, handle string) *identity.Store {
	t.Helper()
	st, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Link("github:"+numericID, canonical, handle); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestResolveLinkedGitHubLogin(t *testing.T) {
	if got := ResolveLinkedGitHubLogin(nil, "1"); got != "" {
		t.Fatalf("nil store: %q", got)
	}
	empty, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveLinkedGitHubLogin(empty, "42"); got != "" {
		t.Fatalf("nothing linked: %q", got)
	}

	// The link records the handle with the @ already stripped, and the lookup is
	// keyed on the ACCOUNT rather than on any one of its logins.
	links := linkedIdentity(t, "42", "777", "@alice-gh")
	if got := ResolveLinkedGitHubLogin(links, "42"); got != "alice-gh" {
		t.Fatalf("linked: %q", got)
	}
	if got := ResolveLinkedGitHubLogin(links, "99"); got != "" {
		t.Fatalf("another account must not borrow the link: %q", got)
	}
	// A GitHub login linked without a handle attributes to nobody, so it must
	// read as unlinked rather than produce a bare "@".
	noHandle, err := identity.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := noHandle.Link("github:5", "u5", ""); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLinkedGitHubLogin(noHandle, "u5"); got != "" {
		t.Fatalf("handle-less link must stay unattributed: %q", got)
	}
}

func TestRequestFormalGitHubReviewLinked(t *testing.T) {
	dir := t.TempDir()
	links := linkedIdentity(t, "rev1", "777", "bob-gh")

	var calls []string
	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("ok"), nil
	}
	login, err := requestFormalGitHubReview(context.Background(), run, links, dir, "acme", "app", 9, "rev1")
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
		t.Fatalf("want linked reviewer: %s", calls[0])
	}
	if !strings.Contains(calls[0], "--repo acme/app") {
		t.Fatalf("want repo: %s", calls[0])
	}
}

func TestRequestFormalGitHubReviewUnlinkedNoGH(t *testing.T) {
	dir := t.TempDir()
	links := linkedIdentity(t, "somebody-else", "777", "bob-gh")

	calls := 0
	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("should not run")
	}
	login, err := requestFormalGitHubReview(context.Background(), run, links, dir, "acme", "app", 9, "nobody")
	if err != nil {
		t.Fatalf("unlinked should not error: %v", err)
	}
	if login != "" {
		t.Fatalf("login=%q", login)
	}
	if calls != 0 {
		t.Fatalf("gh must not run for an unlinked reviewer, calls=%d", calls)
	}
}

func TestRequestFormalGitHubReviewGHError(t *testing.T) {
	dir := t.TempDir()
	links := linkedIdentity(t, "r", "12", "x")

	run := func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		return nil, errors.New("gh: user not found")
	}
	login, err := requestFormalGitHubReview(context.Background(), run, links, dir, "o", "r", 1, "r")
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

// The help text has to point at the thing the user can actually do. There is no
// admin GitHub map to be added to any more, so telling somebody to ask for one
// sends them to a config page that no longer exists.
func TestReviewHelpPointsAtLinkingNotAConfigMap(t *testing.T) {
	h := reviewHelpText()
	if !strings.Contains(h, "GitHub") {
		t.Fatalf("help should mention GitHub: %s", h)
	}
	if !strings.Contains(h, "linked") {
		t.Fatalf("help should say the login must be linked: %s", h)
	}
	if strings.Contains(strings.ToLower(h), "map") {
		t.Fatalf("help must not send the user to a removed GitHub map: %s", h)
	}
}
