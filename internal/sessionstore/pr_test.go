package sessionstore

import (
	"testing"
)

func TestNormalizeLegacyAndUpsertMulti(t *testing.T) {
	e := Entry{
		SessionID: "s",
		PRURL:     "https://github.com/o/r/pull/5",
		PRNumber:  5,
		PRState:   "OPEN",
	}
	e.NormalizePRs()
	if len(e.PRs) != 1 || e.PRs[0].Number != 5 || e.PRs[0].Owner != "o" {
		t.Fatalf("legacy migrate: %+v", e.PRs)
	}

	e.UpsertPR(TrackedPR{URL: "https://github.com/o/other/pull/9", Number: 9, State: "OPEN"})
	if len(e.PRs) != 2 {
		t.Fatalf("want 2 prs got %d", len(e.PRs))
	}
	// Update first PR state.
	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/5", Number: 5, State: "MERGED", Checks: "✓ 1"})
	if len(e.PRs) != 2 {
		t.Fatalf("upsert grew list: %d", len(e.PRs))
	}
	p, ok := e.FindPR("https://github.com/o/r/pull/5")
	if !ok || p.State != "MERGED" || p.Checks != "✓ 1" {
		t.Fatalf("find/update: %+v ok=%v", p, ok)
	}
	if e.AllPRsTerminal() {
		t.Fatal("other PR still open")
	}
	e.UpsertPR(TrackedPR{URL: "https://github.com/o/other/pull/9", Number: 9, State: "CLOSED"})
	if !e.AllPRsTerminal() {
		t.Fatal("expected all terminal")
	}
}

func TestFindPRByNumberUnique(t *testing.T) {
	e := Entry{}
	e.UpsertPR(TrackedPR{URL: "https://github.com/o/r/pull/3", Number: 3, State: "OPEN"})
	p, ok := e.FindPR("3")
	if !ok || p.Number != 3 {
		t.Fatalf("%+v %v", p, ok)
	}
	p, ok = e.FindPR("o/r#3")
	if !ok {
		t.Fatal("slug find")
	}
	_ = p
}

func TestPreserveCIOnUpsert(t *testing.T) {
	e := Entry{}
	e.UpsertPR(TrackedPR{
		URL: "https://github.com/o/r/pull/1", Number: 1, State: "OPEN",
		CINotifiedSHA: "abc", CIAutoFixCount: 2, StatusMsgID: "m1",
	})
	e.UpsertPR(TrackedPR{
		URL: "https://github.com/o/r/pull/1", Number: 1, State: "OPEN", Checks: "✗ 1",
	})
	p, _ := e.FindPR("https://github.com/o/r/pull/1")
	if p.CINotifiedSHA != "abc" || p.CIAutoFixCount != 2 || p.StatusMsgID != "m1" {
		t.Fatalf("lost CI/card fields: %+v", p)
	}
	if p.Checks != "✗ 1" {
		t.Fatalf("checks not updated: %+v", p)
	}
}
