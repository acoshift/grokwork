package bot

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const extraNote = "watch the #nil? path & retries"

func TestExtraInstructionsOnPromptBuilders(t *testing.T) {
	t.Parallel()
	tracked := sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys,
		ID:       "iss1", Title: "nil map",
		Location: "loc", Resource: "api",
	}
	actor := Actor{ID: "u1", DisplayName: "Ada"}
	cases := []struct {
		name   string
		build  func(extra string) string
		canned []string
	}{
		{
			name: "commit review",
			build: func(extra string) string {
				return BuildCommitReviewPrompt(CommitReviewOpts{
					Actor: actor, Owner: "acme", Repo: "app",
					SHA: "abcdef0123456789", Subject: "fix nil",
					Extra: extra,
				})
			},
			canned: []string{"Do not merge anything", "gh issue create"},
		},
		{
			name: "pr review",
			build: func(extra string) string {
				return BuildPRReviewPrompt(PRReviewOpts{
					Actor: actor, Owner: "acme", Repo: "app", Number: 9,
					Title: "CI PR", Extra: extra,
				})
			},
			canned: []string{"never merge", "gh pr comment", "Do not open GitHub issues"},
		},
		{
			name: "address ci",
			build: func(extra string) string {
				return BuildAddressCIPrompt(AddressCIOpts{
					Actor: actor, Owner: "acme", Repo: "app", Number: 9,
					Title: "CI PR", HeadRef: "feat",
					Failed: []ghpr.Check{{Name: "ci", Bucket: "fail"}},
					Extra:  extra,
				})
			},
			canned: []string{"Do not merge the PR."},
		},
		{
			name: "address review",
			build: func(extra string) string {
				return BuildAddressReviewPrompt(AddressReviewOpts{
					Actor: actor, Owner: "acme", Repo: "app", Number: 9,
					Title:    "CI PR",
					Comments: []ghpr.ReviewComment{{Path: "main.go", Line: 12, Body: "please handle nil", Author: "rev"}},
					Extra:    extra,
				})
			},
			canned: []string{"Do not merge the PR.", "please handle nil"},
		},
		{
			name: "github fix",
			build: func(extra string) string {
				return BuildGitHubFixPrompt("Ada", "acme", "app", 7, "Bug", "https://github.com/acme/app/issues/7", "body text", false, extra)
			},
			canned: []string{"Do not merge.", "acme/app#7"},
		},
		{
			name: "linear fix",
			build: func(extra string) string {
				return BuildLinearFixPrompt("Ada", "ENG-9", "Title", "https://linear.app/x/issue/ENG-9", "Todo", "desc", false, extra)
			},
			canned: []string{"Do not merge.", "linear_get_issue"},
		},
		{
			name: "clickup fix",
			build: func(extra string) string {
				return BuildClickUpFixPrompt("Ada", "DEV-9", "Title", "https://app.clickup.com/t/999/DEV-9", "open", "desc", false, extra)
			},
			canned: []string{"Do not merge.", "clickup_get_task"},
		},
		{
			name: "error fix",
			build: func(extra string) string {
				return BuildErrorFixPrompt("Ada", tracked, false, extra)
			},
			canned: []string{"Do not merge.", "deploys_errors_get"},
		},
	}
	long := strings.Repeat("あ", fixPromptBodyMaxRunes+80)
	for _, tc := range cases {
		base := tc.build("")
		if got := tc.build("   \n\t  "); got != base {
			t.Fatalf("%s: whitespace-only extra changed the prompt", tc.name)
		}
		if strings.Contains(base, "### Additional instructions") {
			t.Fatalf("%s: omitted extra emitted a section", tc.name)
		}
		with := tc.build(extraNote)
		if !strings.Contains(with, extraNote) {
			t.Fatalf("%s: missing extra text", tc.name)
		}
		if strings.Contains(with, "&amp;") || strings.Contains(with, "&#35;") || strings.Contains(with, "&amp;amp;") {
			t.Fatalf("%s: extra text was HTML-escaped:\n%s", tc.name, with)
		}
		section := strings.Index(with, "\n### Additional instructions\n")
		if section < 0 {
			t.Fatalf("%s: missing additional-instructions section", tc.name)
		}
		for _, canned := range tc.canned {
			at := strings.Index(with, canned)
			if at < 0 || at > section {
				t.Fatalf("%s: canned %q missing before the extra section", tc.name, canned)
			}
		}
		if !strings.HasPrefix(with, base) {
			t.Fatalf("%s: extra note rewrote the canned prompt", tc.name)
		}
		over := tc.build(long)
		if !strings.HasPrefix(over, base) {
			t.Fatalf("%s: over-long extra rewrote the canned prompt", tc.name)
		}
		if strings.Contains(over, long) {
			t.Fatalf("%s: over-long extra was not shortened", tc.name)
		}
		body := strings.TrimPrefix(over, base+"\n### Additional instructions\n")
		body = strings.TrimSuffix(body, "\n")
		if got := len([]rune(body)); got != fixPromptBodyMaxRunes {
			t.Fatalf("%s: truncated extra is %d runes, want %d", tc.name, got, fixPromptBodyMaxRunes)
		}
		if !strings.HasSuffix(body, "…") {
			t.Fatalf("%s: truncated extra missing ellipsis", tc.name)
		}
	}
}

func TestExtraIgnoredForImplementAndInvestigate(t *testing.T) {
	t.Parallel()
	impl := fixPromptFor(FixStartOpts{
		Kind: FixKindGitHub, UseGoal: true, Extra: extraNote,
		Owner: "acme", Repo: "app", Number: 42,
		Title: "Auth SSO", Body: "## Breakdown\n- [ ] one\n",
		Actor: Actor{DisplayName: "Ada"},
	}, false, false)
	if strings.Contains(impl, extraNote) || strings.Contains(impl, "### Additional instructions") {
		t.Fatalf("implement prompt took the extra note:\n%s", impl)
	}
	if !strings.Contains(impl, "/goal") || !strings.Contains(impl, "Do not merge") {
		t.Fatalf("implement prompt lost its canned body:\n%s", impl)
	}

	tracked := sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys,
		ID:       "iss1", Title: "nil map", Location: "loc", Resource: "api",
	}
	inv := errorPromptFor(ErrorStartOpts{
		Intent: ErrorIntentInvestigate, Extra: extraNote,
		Actor: Actor{DisplayName: "Ada"},
	}, tracked, false)
	if strings.Contains(inv, extraNote) || strings.Contains(inv, "### Additional instructions") {
		t.Fatalf("investigate prompt took the extra note:\n%s", inv)
	}
	if !strings.Contains(inv, "Investigate production error") {
		t.Fatalf("investigate prompt:\n%s", inv)
	}
}

func TestStartFixAndErrorCarryExtraOnReuseAndCreate(t *testing.T) {
	b, _ := testFixBot(t)
	t.Cleanup(func() { WaitIdleForTest(b, 5*time.Second) })
	if err := b.cfg.SetProjectClickUp("app", true, "ws", "list1", "DEV", "cu-key", false); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	fake := &FakeThreadAPI{NextMsg: "m1", NextTh: "th-extra"}
	b.threadAPI = fake

	var got StartTaskOpts
	b.startTaskHook = func(opts StartTaskOpts) { got = opts }
	actor := Actor{ID: "u1", DisplayName: "Ada"}

	gh := sessionstore.Entry{Project: "app", Goal: "keep github goal"}
	gh.UpsertIssue(sessionstore.TrackedIssue{
		Owner: "acme", Repo: "app", Number: 7, Keyword: sessionstore.IssueKeywordFixes,
	})
	if err := b.sessions.Set("gh-reuse", gh); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ThreadID: "gh-reuse",
		Owner: "acme", Repo: "app", Number: 7, Title: "Bug", Body: "body",
		Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge."})
	if e, ok := b.sessions.Get("gh-reuse"); !ok || e.Goal != "keep github goal" || strings.Contains(e.Goal, extraNote) {
		t.Fatalf("reuse goal=%q", e.Goal)
	}

	lin := sessionstore.Entry{Project: "app", Goal: "keep linear goal"}
	lin.UpsertIssue(sessionstore.TrackedIssue{
		Provider: sessionstore.ProviderLinear, Identifier: "ENG-9",
		Keyword: sessionstore.IssueKeywordFixes,
	})
	if err := b.sessions.Set("lin-reuse", lin); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindLinear, Project: "app", ThreadID: "lin-reuse",
		Identifier: "ENG-9", Title: "Title", Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge.", "linear_get_issue"})
	if e, ok := b.sessions.Get("lin-reuse"); !ok || e.Goal != "keep linear goal" {
		t.Fatalf("linear goal=%q", e.Goal)
	}

	cu := sessionstore.Entry{Project: "app", Goal: "keep clickup goal"}
	cu.UpsertIssue(sessionstore.TrackedIssue{
		Provider: sessionstore.ProviderClickUp, ClickUpID: "abc", CustomID: "DEV-9",
		Keyword: sessionstore.IssueKeywordFixes,
	})
	if err := b.sessions.Set("cu-reuse", cu); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindClickUp, Project: "app", ThreadID: "cu-reuse",
		ClickUpID: "abc", CustomID: "DEV-9", Title: "Task",
		Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge.", "clickup_get_task"})

	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true, UseGoal: true,
		Owner: "acme", Repo: "app", Number: 42, Title: "Auth SSO",
		Body: "## Breakdown\n- [ ] one\n", Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Prompt, extraNote) || strings.Contains(got.Prompt, "### Additional instructions") {
		t.Fatalf("implement start took the extra note:\n%s", got.Prompt)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.Prompt), "/goal") {
		t.Fatalf("implement prompt=%q", got.Prompt)
	}

	errEnt := sessionstore.Entry{Project: "app", Goal: "keep error goal"}
	if err := errEnt.UpsertError(sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
		Location: "loc", Resource: "api", Title: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.sessions.Set("err-reuse", errEnt); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentFix,
		Project: "app", ThreadID: "err-reuse",
		ID: "iss1", Location: "loc", Resource: "api", Title: "boom",
		Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge.", "deploys_errors_get"})
	if e, ok := b.sessions.Get("err-reuse"); !ok || e.Goal != "keep error goal" || strings.Contains(e.Goal, extraNote) {
		t.Fatalf("error reuse goal=%q", e.Goal)
	}

	if _, err := b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentInvestigate,
		Project: "app", ForceNew: true,
		ID: "iss2", Location: "loc", Resource: "api", Title: "other",
		Extra: extraNote, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Prompt, extraNote) || strings.Contains(got.Prompt, "### Additional instructions") {
		t.Fatalf("investigate start took the extra note:\n%s", got.Prompt)
	}

	before := fake.StartCount()
	res, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 8, Title: "Pay bug",
		URL:   "https://github.com/acme/app/issues/8",
		Extra: extraNote, Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge."})
	if fake.StartCount() != before+1 {
		t.Fatalf("creates=%d", fake.StartCount())
	}
	joined := strings.Join(fake.Starts, "\n") + "\n" + strings.Join(fake.Sends, "\n")
	if strings.Contains(joined, extraNote) {
		t.Fatalf("discord title or starter contains the extra note:\n%s", joined)
	}
	if e, ok := b.sessions.Get(res.ThreadID); !ok || strings.Contains(e.Goal, extraNote) {
		t.Fatalf("create goal=%q", e.Goal)
	}

	before = fake.StartCount()
	res, err = b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentFix,
		Project: "app", ForceNew: true,
		ID: "iss3", Location: "loc", Resource: "api", Title: "boom",
		URL:   "https://example.test/errors/iss3",
		Extra: extraNote, Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge."})
	joined = strings.Join(fake.Starts[before:], "\n") + "\n" + strings.Join(fake.Sends, "\n")
	if strings.Contains(joined, extraNote) {
		t.Fatalf("error discord title or starter contains the extra note:\n%s", joined)
	}
	if e, ok := b.sessions.Get(res.ThreadID); !ok || strings.Contains(e.Goal, extraNote) || !strings.Contains(e.Goal, "boom") {
		t.Fatalf("error goal=%q", e.Goal)
	}

	// Whitespace-only matches a create with the field omitted. Capture both prompts.
	var plain, blank string
	b.startTaskHook = func(opts StartTaskOpts) {
		if opts.Prompt != "" && plain == "" {
			plain = opts.Prompt
			return
		}
		blank = opts.Prompt
	}
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 11, Title: "Same", Body: "body",
		Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 11, Title: "Same", Body: "body",
		Extra: " \n\t ", Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	if plain == "" || plain != blank {
		t.Fatalf("whitespace extra changed the enqueued prompt\nplain=%q\nblank=%q", plain, blank)
	}

	long := strings.Repeat("あ", fixPromptBodyMaxRunes+40)
	b.startTaskHook = func(opts StartTaskOpts) { got = opts }
	if _, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 12, Title: "Long", Body: "body",
		Extra: long, Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Prompt, "Do not merge.") || strings.Contains(got.Prompt, long) {
		t.Fatal("over-long extra damaged the canned fix prompt")
	}
}

func TestExtraDoesNotBypassStartOrModelGate(t *testing.T) {
	b, _ := testFixBot(t)
	if err := b.cfg.SetProjectCapabilityByUser("app", "inv", "investigator"); err != nil {
		t.Fatal(err)
	}
	called := false
	b.startTaskHook = func(StartTaskOpts) { called = true }
	actor := Actor{ID: "inv", DisplayName: "Inv"}
	_, err := b.StartFix(FixStartOpts{
		Kind: FixKindGitHub, Project: "app", ForceNew: true,
		Owner: "acme", Repo: "app", Number: 7, Title: "Bug",
		Extra: extraNote, Actor: actor,
	})
	if !errors.Is(err, ErrCannotStartFix) {
		t.Fatalf("fix gate err=%v", err)
	}
	_, err = b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentFix,
		Project: "app", ID: "iss1", Location: "loc", Resource: "api",
		Extra: extraNote, Actor: actor,
	})
	// Provider is off, so the provider check wins before the ship gate. Enable it
	// and confirm the ship gate still refuses.
	if err := b.cfg.SetProjectErrorsDeploys("app", true, "acme", "loc", "api", "tok", false); err != nil {
		t.Fatal(err)
	}
	_, err = b.StartError(ErrorStartOpts{
		Provider: errsrc.ProviderDeploys, Intent: ErrorIntentFix,
		Project: "app", ID: "iss1", Location: "loc", Resource: "api",
		Extra: extraNote, Actor: actor,
	})
	if !errors.Is(err, ErrCannotStartFix) {
		t.Fatalf("error fix gate err=%v", err)
	}
	_, err = b.StartCommitReview(CommitReviewOpts{
		Project: "app", Actor: actor,
		Owner: "acme", Repo: "app", SHA: "abcdef0123456789",
		Model: "claude-opus-5-high", Extra: extraNote,
	})
	if !errors.Is(err, ErrCannotSelectModel) {
		t.Fatalf("model gate err=%v", err)
	}
	_, err = b.StartPRReview(PRReviewOpts{
		Project: "app", Actor: actor,
		Owner: "acme", Repo: "app", Number: 9,
		Model: "claude-opus-5-high", Extra: extraNote,
	})
	if !errors.Is(err, ErrCannotSelectModel) {
		t.Fatalf("pr model gate err=%v", err)
	}
	if called {
		t.Fatal("a refused start enqueued a run")
	}

	// Empty model stays allowed. The extra note still reaches the prompt.
	var got StartTaskOpts
	b.startTaskHook = func(opts StartTaskOpts) { got = opts }
	if _, err := b.StartCommitReview(CommitReviewOpts{
		Project: "app", Actor: actor,
		Owner: "acme", Repo: "app", SHA: "abcdef0123456789", Subject: "fix nil",
		Extra: extraNote,
	}); err != nil {
		t.Fatal(err)
	}
	assertPromptHasExtra(t, got.Prompt, []string{"Do not merge anything"})
	if e, ok := b.sessions.Get(got.ThreadID); ok && strings.Contains(e.Goal, extraNote) {
		t.Fatalf("review goal=%q", e.Goal)
	}
}

func assertPromptHasExtra(t *testing.T, prompt string, canned []string) {
	t.Helper()
	if !strings.Contains(prompt, extraNote) {
		t.Fatalf("prompt missing extra:\n%s", prompt)
	}
	if strings.Contains(prompt, "&amp;") {
		t.Fatalf("prompt HTML-escaped the extra note:\n%s", prompt)
	}
	section := strings.Index(prompt, "### Additional instructions")
	if section < 0 {
		t.Fatalf("prompt missing section:\n%s", prompt)
	}
	for _, c := range canned {
		at := strings.Index(prompt, c)
		if at < 0 || at > section {
			t.Fatalf("canned %q not before extra section:\n%s", c, prompt)
		}
	}
}
