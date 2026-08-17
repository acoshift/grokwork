package bot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestStartFixStagesGitHubImages(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })

	const asset = "https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var sawURL string
	b.githubAssetGet = func(_ context.Context, rawURL string) (string, []byte, error) {
		sawURL = rawURL
		return "image/png", tinyPNG, nil
	}
	var got StartTaskOpts
	var stagedOK bool
	b.startTaskHook = func(opts StartTaskOpts) {
		got = opts
		if len(opts.AttachmentPaths) == 1 {
			if _, err := os.Stat(opts.AttachmentPaths[0]); err == nil {
				stagedOK = true
			}
		}
	}

	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 9,
		Title: "Screenshot bug",
		Body:  `see <img src="` + asset + `" alt="repro">`,
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID == "" {
		t.Fatal("expected a session")
	}
	if sawURL != asset {
		t.Fatalf("fetched %q", sawURL)
	}
	if len(got.AttachmentPaths) != 1 {
		t.Fatalf("attachments=%v", got.AttachmentPaths)
	}
	if !stagedOK {
		t.Fatal("staged file missing at StartTask")
	}
	if !strings.Contains(got.AttachmentPaths[0], "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Fatalf("name=%q", got.AttachmentPaths[0])
	}
}

func TestStartFixImageFetchFailureStillStarts(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	b.githubAssetGet = func(context.Context, string) (string, []byte, error) {
		return "", nil, context.DeadlineExceeded
	}
	var got StartTaskOpts
	b.startTaskHook = func(opts StartTaskOpts) { got = opts }

	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 10,
		Title: "T", Body: "![x](https://github.com/user-attachments/assets/abcd)",
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID == "" {
		t.Fatal("expected a session")
	}
	if len(got.AttachmentPaths) != 0 {
		t.Fatalf("failed fetch must not attach: %v", got.AttachmentPaths)
	}
}

func TestStartFixPickerDoesNotFetchImages(t *testing.T) {
	b, _ := testFixBot(t)
	called := false
	b.githubAssetGet = func(context.Context, string) (string, []byte, error) {
		called = true
		return "image/png", tinyPNG, nil
	}
	for _, id := range []string{"p1", "p2"} {
		e := sessionstore.Entry{Project: "app"}
		e.UpsertIssue(sessionstore.TrackedIssue{Owner: "acme", Repo: "app", Number: 3, Keyword: sessionstore.IssueKeywordFixes})
		if err := b.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 3,
		Title: "T", Body: "![x](https://github.com/user-attachments/assets/abcd)",
		Actor: Actor{ID: "u", DisplayName: "U"},
	})
	if err == nil {
		t.Fatal("expected picker")
	}
	if called {
		t.Fatal("picker must not fetch images")
	}
}

func TestStartFixImageTextComments(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	const asset = "https://github.com/user-attachments/assets/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	var saw string
	b.githubAssetGet = func(_ context.Context, rawURL string) (string, []byte, error) {
		saw = rawURL
		return "image/png", tinyPNG, nil
	}
	var got StartTaskOpts
	b.startTaskHook = func(opts StartTaskOpts) { got = opts }

	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app",
		Owner: "acme", Repo: "app", Number: 11,
		Title:     "T",
		Body:      "no image here",
		ImageText: "no image here\ncomment ![c](" + asset + ")",
		Actor:     Actor{ID: "u", DisplayName: "U"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saw != asset {
		t.Fatalf("comment image not fetched: %q", saw)
	}
	if len(got.AttachmentPaths) != 1 {
		t.Fatalf("attachments=%v", got.AttachmentPaths)
	}
}
