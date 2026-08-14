package bot

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestIssueBindingPrompt(t *testing.T) {
	p := issueBindingPrompt(nil)
	if p != "" {
		t.Fatalf("empty: %q", p)
	}
	p = issueBindingPrompt([]sessionstore.TrackedIssue{
		{Number: 42, Keyword: sessionstore.IssueKeywordFixes, Owner: "o", Repo: "r"},
		{Number: 7, Keyword: sessionstore.IssueKeywordRefs},
	})
	for _, want := range []string{
		"Linked tickets",
		"Fixes o/r#42",
		"Refs #7",
		"Prefix the PR title",
		"#42 #7",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
}

func TestParseLinkArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/link", ""},
		{"link", ""},
		{"/link #42", "#42"},
		{"/link fix #42", "fix #42"},
		{"/unlink #9", "unlink #9"},
		{"unlink 9", "unlink 9"},
	}
	for _, tc := range cases {
		if got := parseLinkArg(tc.in); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitLinkKeyword(t *testing.T) {
	kw, rest := splitLinkKeyword("fix #42")
	if kw != sessionstore.IssueKeywordFixes || rest != "#42" {
		t.Fatalf("got %q %q", kw, rest)
	}
	kw, rest = splitLinkKeyword("refs o/r#3")
	if kw != sessionstore.IssueKeywordRefs || rest != "o/r#3" {
		t.Fatalf("got %q %q", kw, rest)
	}
	kw, rest = splitLinkKeyword("#99")
	if kw != "" || rest != "#99" {
		t.Fatalf("got %q %q", kw, rest)
	}
}

func TestPrefixThreadTitleWithIssues(t *testing.T) {
	issues := []sessionstore.TrackedIssue{{Number: 42}}
	got := prefixThreadTitleWithIssues("fix payment timeout", issues)
	if got != "#42 fix payment timeout" {
		t.Fatalf("got %q", got)
	}
	// Idempotent
	got = prefixThreadTitleWithIssues("#42 fix payment timeout", issues)
	if got != "#42 fix payment timeout" {
		t.Fatalf("double: %q", got)
	}
}

func TestPreserveIssueFields(t *testing.T) {
	prev := sessionstore.Entry{
		Issues: []sessionstore.TrackedIssue{{Number: 5, Keyword: sessionstore.IssueKeywordFixes}},
	}
	next := sessionstore.Entry{SessionID: "s"}
	preserveIssueFields(&next, prev)
	if len(next.Issues) != 1 || next.Issues[0].Number != 5 {
		t.Fatalf("got %+v", next.Issues)
	}
	// Do not clobber.
	next2 := sessionstore.Entry{Issues: []sessionstore.TrackedIssue{{Number: 9}}}
	preserveIssueFields(&next2, prev)
	if next2.Issues[0].Number != 9 {
		t.Fatalf("clobber: %+v", next2.Issues)
	}
}

func TestBindIssuesFromText(t *testing.T) {
	b := testBot(t)
	if err := b.sessions.Set("t1", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	bound := b.bindIssuesFromText("t1", "please fix #88 in auth", "acoshift", "grokwork")
	if len(bound) != 1 {
		t.Fatalf("bound=%v", bound)
	}
	e, _ := b.sessions.Get("t1")
	if !e.HasIssues() || e.Issues[0].Number != 88 {
		t.Fatalf("%+v", e.Issues)
	}
	if e.Issues[0].EffectiveKeyword() != sessionstore.IssueKeywordFixes {
		t.Fatalf("keyword=%s", e.Issues[0].Keyword)
	}
	if e.Issues[0].Owner != "acoshift" {
		t.Fatalf("owner=%s", e.Issues[0].Owner)
	}
}

func TestIssueBindingPromptLinear(t *testing.T) {
	p := issueBindingPrompt([]sessionstore.TrackedIssue{
		{Provider: sessionstore.ProviderLinear, Identifier: "ENG-123", Keyword: sessionstore.IssueKeywordFixes, Title: "Auth timeout", State: "In Progress", URL: "https://linear.app/x/issue/ENG-123"},
	})
	for _, want := range []string{"ENG-123", "Fixes ENG-123", "In Progress", "Auth timeout", "eng-123"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
}

func TestBindLinearIssuesRespectsOptIn(t *testing.T) {
	b := testBot(t)
	if err := b.sessions.Set("t2", sessionstore.Entry{Project: "app"}); err != nil {
		t.Fatal(err)
	}
	// Linear disabled by default on test bot projects.
	bound := b.bindLinearIssuesFromText("t2", "app", "fix ENG-99 please")
	if len(bound) != 0 {
		t.Fatalf("expected no bind when disabled: %v", bound)
	}
	// Enable Linear for app without API key.
	if b.cfg.Projects == nil {
		b.cfg.Projects = config.ProjectsMap{}
	}
	b.cfg.Projects["app"] = config.ProjectConfig{
		Path:   "/tmp/app",
		Linear: &config.ProjectLinearConfig{Enabled: true, TeamKey: "ENG"},
	}
	bound = b.bindLinearIssuesFromText("t2", "app", "fix ENG-99 please")
	if len(bound) != 1 || bound[0].Identifier != "ENG-99" {
		t.Fatalf("bound=%+v", bound)
	}
	e, _ := b.sessions.Get("t2")
	if !e.Issues[0].IsLinear() || e.Issues[0].Identifier != "ENG-99" {
		t.Fatalf("%+v", e.Issues)
	}
}

func TestBindClickUpAndLinearCoexistence(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{
			"app": {
				Path: dir,
				Linear: &config.ProjectLinearConfig{
					Enabled: true,
					TeamKey: "ENG",
				},
				ClickUp: &config.ProjectClickUpConfig{
					Enabled:        true,
					CustomIdPrefix: "DEV",
				},
			},
		},
	}
	b := &Bot{cfg: cfg, sessions: store}
	// DEV claimed by ClickUp; ENG stays Linear.
	bound := b.bindLinearIssuesFromText("t1", "app", "fix DEV-12 and ENG-9")
	for _, iss := range bound {
		if iss.IsLinear() && strings.HasPrefix(iss.Identifier, "DEV") {
			t.Fatalf("DEV should not bind as Linear: %+v", iss)
		}
	}
	if len(bound) != 1 || bound[0].Identifier != "ENG-9" {
		t.Fatalf("linear should only bind ENG-9: %+v", bound)
	}
	b.bindClickUpIssuesFromText("t1", "app", "fix DEV-12 and ENG-9")
	e, ok := store.Get("t1")
	if !ok {
		t.Fatal("missing session")
	}
	var hasCU, hasENG bool
	for _, iss := range e.Issues {
		if iss.IsClickUp() && iss.CustomID == "DEV-12" {
			hasCU = true
		}
		if iss.IsLinear() && iss.Identifier == "ENG-9" {
			hasENG = true
		}
		if iss.IsLinear() && strings.HasPrefix(iss.Identifier, "DEV") {
			t.Fatalf("linear DEV leak: %+v", iss)
		}
	}
	if !hasCU || !hasENG {
		t.Fatalf("want both DEV-12 clickup and ENG-9 linear; got %+v", e.Issues)
	}
	// Unlink ClickUp custom id via provider-agnostic query
	if !e.RemoveIssue("DEV-12") {
		t.Fatal("unlink DEV-12")
	}
	if _, ok := e.FindIssue("DEV-12"); ok {
		t.Fatal("DEV-12 still present")
	}
}

func TestIssueBindingPromptClickUp(t *testing.T) {
	p := issueBindingPrompt([]sessionstore.TrackedIssue{
		{Provider: sessionstore.ProviderClickUp, CustomID: "DEV-3", Keyword: sessionstore.IssueKeywordFixes, Title: "Auth", State: "open", URL: "https://app.clickup.com/t/x"},
	})
	if !strings.Contains(p, "Fixes DEV-3") {
		t.Fatalf("missing Fixes line: %s", p)
	}
	if !strings.Contains(p, "clickup_get_task") {
		t.Fatalf("missing mcp tool: %s", p)
	}
	if strings.Contains(p, "Linear's") || strings.Contains(p, "Linear state") {
		t.Fatalf("must not claim Linear integration for ClickUp-only: %s", p)
	}
}

func TestBindClickUpURLDoesNotDualBindLinear(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{
			"app": {
				Path: dir,
				Linear: &config.ProjectLinearConfig{
					Enabled: true,
					TeamKey: "ENG",
				},
				// URL-only ClickUp (no prefix) — pasting custom-id URL must not also bind Linear.
				ClickUp: &config.ProjectClickUpConfig{
					Enabled: true,
				},
			},
		},
	}
	b := &Bot{cfg: cfg, sessions: store}
	text := "fix https://app.clickup.com/t/1234567/DEV-9 please"
	b.bindLinearIssuesFromText("t1", "app", text)
	b.bindClickUpIssuesFromText("t1", "app", text)
	e, _ := store.Get("t1")
	var linDEV, cuDEV bool
	for _, iss := range e.Issues {
		if iss.IsLinear() && strings.HasPrefix(iss.Identifier, "DEV") {
			linDEV = true
		}
		if iss.IsClickUp() && iss.CustomID == "DEV-9" {
			cuDEV = true
		}
	}
	if !cuDEV {
		t.Fatalf("want ClickUp DEV-9; got %+v", e.Issues)
	}
	if linDEV {
		t.Fatalf("Linear must not claim DEV from ClickUp URL: %+v", e.Issues)
	}
}

func TestBindLinearURLNotStolenByClickUpPrefix(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Projects: config.ProjectsMap{
			"app": {
				Path: dir,
				Linear: &config.ProjectLinearConfig{
					Enabled: true,
					TeamKey: "DEV",
				},
				ClickUp: &config.ProjectClickUpConfig{
					Enabled:        true,
					CustomIdPrefix: "DEV", // collision: prefix-parse off, Linear wins bare
				},
			},
		},
	}
	b := &Bot{cfg: cfg, sessions: store}
	// Explicit Linear URL must remain Linear even when ClickUp is enabled.
	text := "see https://linear.app/acme/issue/DEV-7/fix-auth"
	b.bindClickUpIssuesFromText("t1", "app", text)
	b.bindLinearIssuesFromText("t1", "app", text)
	e, _ := store.Get("t1")
	var hasLin, hasCU bool
	for _, iss := range e.Issues {
		if iss.IsLinear() && iss.Identifier == "DEV-7" {
			hasLin = true
		}
		if iss.IsClickUp() {
			hasCU = true
		}
	}
	if !hasLin {
		t.Fatalf("want Linear DEV-7 from URL; got %+v", e.Issues)
	}
	if hasCU {
		t.Fatalf("ClickUp must not claim ids inside Linear URL: %+v", e.Issues)
	}
}
