package sessionstore

import (
	"strings"
	"testing"
)

func TestParseIssueRefsURL(t *testing.T) {
	got := ParseIssueRefs("see https://github.com/acoshift/grokwork/issues/42 please")
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got[0].Number != 42 || got[0].Owner != "acoshift" || got[0].Repo != "grokwork" {
		t.Fatalf("%+v", got[0])
	}
	if got[0].EffectiveKeyword() != IssueKeywordRefs {
		t.Fatalf("keyword=%s", got[0].Keyword)
	}
	if !strings.Contains(got[0].URL, "/issues/42") {
		t.Fatalf("url=%s", got[0].URL)
	}
}

func TestParseIssueRefsBareAndSlug(t *testing.T) {
	got := ParseIssueRefs("fix #99 and also o/r#12")
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	// Order: owner/repo first in scan order depends on regex order — URL none,
	// then owner/repo, then bare. owner/repo#12 then #99.
	byNum := map[int]TrackedIssue{}
	for _, iss := range got {
		byNum[iss.Number] = iss
	}
	if byNum[99].EffectiveKeyword() != IssueKeywordFixes {
		t.Fatalf("#99 keyword=%s want Fixes", byNum[99].Keyword)
	}
	if byNum[12].Owner != "o" || byNum[12].Repo != "r" {
		t.Fatalf("slug: %+v", byNum[12])
	}
}

func TestParseIssueRefsDedupeURLAndBare(t *testing.T) {
	got := ParseIssueRefs("https://github.com/o/r/issues/7 and #7")
	if len(got) != 1 {
		t.Fatalf("want 1 got %v", got)
	}
	if got[0].Owner != "o" || got[0].Number != 7 {
		t.Fatalf("%+v", got[0])
	}
}

func TestParseIssueRefsCloseIntent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"closes #3", IssueKeywordFixes},
		{"fix issue #3", IssueKeywordFixes},
		{"resolved #3", IssueKeywordFixes},
		{"refs #3", IssueKeywordRefs},
		{"regarding #3", IssueKeywordRefs},
		{"look at #3", IssueKeywordRefs},
	}
	for _, tc := range cases {
		got := ParseIssueRefs(tc.in)
		if len(got) != 1 {
			t.Fatalf("%q: got %v", tc.in, got)
		}
		if got[0].EffectiveKeyword() != tc.want {
			t.Fatalf("%q: keyword=%s want %s", tc.in, got[0].EffectiveKeyword(), tc.want)
		}
	}
}

func TestParseIssueRefsSkipsNonIssueNoise(t *testing.T) {
	// PR URLs are not issues.
	got := ParseIssueRefs("see https://github.com/o/r/pull/5 and color #ff00aa")
	if len(got) != 0 {
		t.Fatalf("unexpected: %v", got)
	}
	// Hex-like mid-token should not match bare issue (no digit-only after #).
	got = ParseIssueRefs("use #abc123")
	if len(got) != 0 {
		t.Fatalf("hex: %v", got)
	}
}

func TestUpsertIssueMergeKeyword(t *testing.T) {
	var e Entry
	e.UpsertIssue(TrackedIssue{Number: 1, Keyword: IssueKeywordRefs})
	e.UpsertIssue(TrackedIssue{Number: 1, Owner: "o", Repo: "r", Keyword: IssueKeywordRefs})
	if len(e.Issues) != 1 {
		t.Fatalf("dedupe: %v", e.Issues)
	}
	if e.Issues[0].Owner != "o" {
		t.Fatalf("fill owner: %+v", e.Issues[0])
	}
	e.UpsertIssue(TrackedIssue{Number: 1, Keyword: IssueKeywordFixes})
	if e.Issues[0].EffectiveKeyword() != IssueKeywordFixes {
		t.Fatalf("upgrade: %+v", e.Issues[0])
	}
	// Do not downgrade Fixes → Refs on re-parse.
	e.UpsertIssue(TrackedIssue{Number: 1, Keyword: IssueKeywordRefs})
	if e.Issues[0].EffectiveKeyword() != IssueKeywordFixes {
		t.Fatalf("downgrade: %+v", e.Issues[0])
	}
	// Explicit force may set Refs.
	e.UpsertIssueForceKeyword(TrackedIssue{Number: 1, Keyword: IssueKeywordRefs})
	if e.Issues[0].EffectiveKeyword() != IssueKeywordRefs {
		t.Fatalf("force refs: %+v", e.Issues[0])
	}
}

func TestRemoveAndClearIssues(t *testing.T) {
	var e Entry
	e.UpsertIssue(TrackedIssue{Number: 1})
	e.UpsertIssue(TrackedIssue{Number: 2, Owner: "o", Repo: "r"})
	if !e.RemoveIssue("#1") {
		t.Fatal("remove #1")
	}
	if e.HasIssues() && len(e.Issues) != 1 {
		t.Fatalf("%v", e.Issues)
	}
	if !e.RemoveIssue("o/r#2") {
		t.Fatal("remove slug")
	}
	if e.HasIssues() {
		t.Fatal("expected empty")
	}
	e.UpsertIssue(TrackedIssue{Number: 9})
	e.ClearIssues()
	if e.HasIssues() {
		t.Fatal("clear")
	}
}

func TestIssueTitlePrefixAndPRBody(t *testing.T) {
	issues := []TrackedIssue{
		{Number: 42, Keyword: IssueKeywordFixes},
		{Number: 7, Keyword: IssueKeywordRefs},
	}
	pref := IssueTitlePrefix(issues)
	if pref != "#42 #7 " {
		t.Fatalf("prefix=%q", pref)
	}
	if issues[0].PRBodyLine() != "Fixes #42" {
		t.Fatalf("body=%q", issues[0].PRBodyLine())
	}
	if issues[1].PRBodyLine() != "Refs #7" {
		t.Fatalf("body=%q", issues[1].PRBodyLine())
	}
	withRepo := TrackedIssue{Number: 3, Owner: "o", Repo: "r", Keyword: IssueKeywordFixes}
	if withRepo.PRBodyLine() != "Fixes o/r#3" {
		t.Fatalf("slug body=%q", withRepo.PRBodyLine())
	}
}

func TestFormatIssueStatusLines(t *testing.T) {
	lines := FormatIssueStatusLines([]TrackedIssue{{Number: 3, Keyword: IssueKeywordFixes, URL: "https://github.com/o/r/issues/3"}})
	if len(lines) != 1 || !strings.Contains(lines[0], "#3") || !strings.Contains(lines[0], "Fixes") {
		t.Fatalf("%v", lines)
	}
	lines = FormatIssueStatusLines([]TrackedIssue{{Number: 1}, {Number: 2}})
	if len(lines) < 3 || !strings.Contains(lines[0], "issues") {
		t.Fatalf("%v", lines)
	}
}

func TestFillIssueOwnerRepo(t *testing.T) {
	issues := []TrackedIssue{{Number: 5}}
	FillIssueOwnerRepo(issues, "acoshift", "grokwork")
	if issues[0].Owner != "acoshift" || !strings.Contains(issues[0].URL, "/issues/5") {
		t.Fatalf("%+v", issues[0])
	}
}
