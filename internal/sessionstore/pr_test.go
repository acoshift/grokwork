package sessionstore

import (
	"testing"
)

func TestDropPRsRefreshesLegacyMirror(t *testing.T) {
	e := Entry{}
	e.UpsertPR(TrackedPR{
		URL: "https://github.com/oven-sh/bun/pull/45631", Number: 45631, State: "OPEN",
		Owner: "oven-sh", Repo: "bun", Title: "aot",
	})
	e.UpsertPR(TrackedPR{
		URL: "https://github.com/acme/app/pull/40", Number: 40, State: "OPEN",
		Owner: "acme", Repo: "app", Title: "bump",
	})
	n := e.DropPRs(func(pr TrackedPR) bool { return pr.Repo == "bun" })
	if n != 1 || len(e.PRs) != 1 || e.PRs[0].Number != 40 {
		t.Fatalf("n=%d prs=%+v", n, e.PRs)
	}
	if e.PRNumber != 40 || e.PRURL != "https://github.com/acme/app/pull/40" {
		t.Fatalf("legacy number=%d url=%s", e.PRNumber, e.PRURL)
	}
	if e.DropPRs(func(TrackedPR) bool { return false }) != 0 {
		t.Fatal("keep-all must not drop")
	}
	if e.DropPRs(func(TrackedPR) bool { return true }) != 1 || len(e.PRs) != 0 || e.PRNumber != 0 || e.PRURL != "" {
		t.Fatalf("cleared prs=%d number=%d url=%q", len(e.PRs), e.PRNumber, e.PRURL)
	}
}

func TestFormatParseAskPRKey(t *testing.T) {
	key := FormatAskPRKey("Acme", "App", 9)
	if key != "acme/app#9" {
		t.Fatalf("key=%q", key)
	}
	owner, repo, n, ok := ParseAskPRKey(key)
	if !ok || owner != "acme" || repo != "app" || n != 9 {
		t.Fatalf("parse %q → %s %s %d ok=%v", key, owner, repo, n, ok)
	}
	if FormatAskPRKey("", "app", 1) != "" || FormatAskPRKey("o", "r", 0) != "" {
		t.Fatal("empty inputs must not format")
	}
	if _, _, _, ok := ParseAskPRKey("not-a-key"); ok {
		t.Fatal("bogus key")
	}
}

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
