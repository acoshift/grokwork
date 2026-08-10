package sessionstore

import (
	"testing"
)

func TestParseClickUpNativeURL(t *testing.T) {
	got := ParseClickUpIssueRefs("see https://app.clickup.com/t/9hx please", "")
	if len(got) != 1 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	if !got[0].IsClickUp() || got[0].ClickUpID != "9hx" {
		t.Fatalf("%+v", got[0])
	}
}

func TestParseClickUpCustomURL(t *testing.T) {
	got := ParseClickUpIssueRefs("fix https://app.clickup.com/t/1234567/DEV-9/slug", "")
	if len(got) != 1 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	iss := got[0]
	if iss.CustomID != "DEV-9" || iss.WorkspaceID != "1234567" {
		t.Fatalf("%+v", iss)
	}
	if iss.ClickUpID != "" {
		t.Fatalf("should not treat workspace as native: %+v", iss)
	}
	if iss.EffectiveKeyword() != IssueKeywordFixes {
		t.Fatalf("keyword=%s", iss.EffectiveKeyword())
	}
}

func TestParseClickUpBarePrefix(t *testing.T) {
	got := ParseClickUpIssueRefs("please fix DEV-12 and ENG-1", "DEV")
	if len(got) != 1 || got[0].CustomID != "DEV-12" {
		t.Fatalf("%+v", got)
	}
	// Without prefix, bare ids ignored.
	if n := ParseClickUpIssueRefs("DEV-12", ""); len(n) != 0 {
		t.Fatalf("expected empty without prefix: %+v", n)
	}
}

func TestClickUpSameIssueAndUnlink(t *testing.T) {
	e := Entry{}
	e.UpsertIssue(TrackedIssue{
		Provider: ProviderClickUp,
		CustomID: "DEV-42",
		Keyword:  IssueKeywordFixes,
	})
	if !e.HasIssues() {
		t.Fatal("expected bind")
	}
	if _, ok := e.FindIssue("DEV-42"); !ok {
		t.Fatal("FindIssue DEV-42")
	}
	if !e.RemoveIssue("DEV-42") {
		t.Fatal("RemoveIssue")
	}
	if e.HasIssues() {
		t.Fatal("expected empty after unlink")
	}
}

func TestClickUpAndLinearFindUnlinkCoexist(t *testing.T) {
	e := Entry{}
	e.UpsertIssue(TrackedIssue{Provider: ProviderClickUp, CustomID: "DEV-1", Keyword: IssueKeywordFixes})
	e.UpsertIssue(TrackedIssue{Provider: ProviderLinear, Identifier: "ENG-1", Keyword: IssueKeywordRefs})
	if _, ok := e.FindIssue("DEV-1"); !ok {
		t.Fatal("find clickup")
	}
	if _, ok := e.FindIssue("ENG-1"); !ok {
		t.Fatal("find linear")
	}
	if !e.RemoveIssue("DEV-1") {
		t.Fatal("unlink clickup")
	}
	if _, ok := e.FindIssue("ENG-1"); !ok {
		t.Fatal("linear should remain")
	}
	if e.RemoveIssue("DEV-1") {
		t.Fatal("already removed")
	}
}

func TestClickUpPRBodyAndTitle(t *testing.T) {
	iss := TrackedIssue{Provider: ProviderClickUp, CustomID: "DEV-2", Keyword: IssueKeywordFixes}
	if line := iss.PRBodyLine(); line != "Fixes DEV-2" {
		t.Fatalf("line=%q", line)
	}
	pref := IssueTitlePrefix([]TrackedIssue{iss})
	if pref != "DEV-2 " {
		t.Fatalf("pref=%q", pref)
	}
}

func TestClickUpDisplayRefNative(t *testing.T) {
	iss := TrackedIssue{Provider: ProviderClickUp, ClickUpID: "9hx"}
	if iss.DisplayRef() != "9hx" {
		t.Fatalf("%q", iss.DisplayRef())
	}
}

func TestParseClickUpBareDoesNotMatchInsideURL(t *testing.T) {
	// Bare prefix must not fire on Linear URL path segments.
	got := ParseClickUpIssueRefs("see https://linear.app/acme/issue/DEV-7/slug", "DEV")
	if len(got) != 0 {
		t.Fatalf("expected no bare match inside Linear URL: %+v", got)
	}
	// Custom ClickUp URL still binds via URL parser.
	got = ParseClickUpIssueRefs("https://app.clickup.com/t/99/DEV-7", "DEV")
	if len(got) != 1 || got[0].CustomID != "DEV-7" {
		t.Fatalf("%+v", got)
	}
}
