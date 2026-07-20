package ghpr

import (
	"context"
	"strings"
	"testing"
)

func TestParseGitHubPRURLs(t *testing.T) {
	text := `
Opened https://github.com/acoshift/grokwork/pull/42 for review.
Also see <https://github.com/acoshift/grokwork/pull/42> and
https://github.com/acoshift/other/pull/7/files
not a pr: https://github.com/acoshift/grokwork/issues/1
`
	got := ParseGitHubPRURLs(text)
	if len(got) != 2 {
		t.Fatalf("len=%d got=%+v", len(got), got)
	}
	if got[0].Number != 42 || got[0].Owner != "acoshift" || got[0].Repo != "grokwork" {
		t.Fatalf("first=%+v", got[0])
	}
	if got[1].Number != 7 || got[1].Repo != "other" {
		t.Fatalf("second=%+v", got[1])
	}
}

func TestSummarizeChecksJSON(t *testing.T) {
	raw := []byte(`[
		{"name":"a","state":"SUCCESS","bucket":"pass"},
		{"name":"b","state":"FAILURE","bucket":"fail"},
		{"name":"c","state":"PENDING","bucket":"pending"},
		{"name":"d","state":"SUCCESS","bucket":"pass"}
	]`)
	sum, err := SummarizeChecksJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "✓ 2 · ✗ 1 · … 1" {
		t.Fatalf("sum=%q", sum)
	}
	empty, err := SummarizeChecksJSON([]byte("[]"))
	if err != nil || empty != "none" {
		t.Fatalf("empty=%q err=%v", empty, err)
	}
	checks, err := ParseChecksJSON(raw)
	if err != nil || !HasFailing(checks) || len(FailedChecks(checks)) != 1 {
		t.Fatalf("checks=%+v err=%v", checks, err)
	}
	digest := FormatCIDigest(42, "abcdef012345", FailedChecks(checks))
	for _, want := range []string{"CI failed", "#42", "abcdef0", "b", "/fix-ci"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q:\n%s", want, digest)
		}
	}
}

func TestFormatCardAndStatus(t *testing.T) {
	info := Info{
		Number:         12,
		URL:            "https://github.com/o/r/pull/12",
		Title:          "Fix timeout",
		State:          "OPEN",
		IsDraft:        true,
		ReviewDecision: "REVIEW_REQUIRED",
		Checks:         "✓ 3 · ✗ 1",
	}
	card := FormatCard(info)
	for _, want := range []string{"#12", "DRAFT", "Fix timeout", "✓ 3", "REVIEW_REQUIRED", "https://github.com/o/r/pull/12"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q:\n%s", want, card)
		}
	}
	lines := FormatStatusLines(info)
	if len(lines) < 2 || !strings.Contains(lines[0], "#12") {
		t.Fatalf("status lines=%v", lines)
	}
}

func TestIsTerminal(t *testing.T) {
	if !IsTerminal("MERGED") || !IsTerminal("closed") {
		t.Fatal("expected terminal")
	}
	if IsTerminal("OPEN") || IsTerminal("") {
		t.Fatal("expected non-terminal")
	}
}

func TestViewWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{"number":9,"url":"https://github.com/o/r/pull/9","title":"T","state":"OPEN","isDraft":false,"reviewDecision":"APPROVED","headRefOid":"abc","headRefName":"grok/discord/1"}`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`), nil
		default:
			t.Fatalf("unexpected args %v", args)
			return nil, nil
		}
	}
	info, err := ViewWith(context.Background(), run, "/tmp", "9")
	if err != nil {
		t.Fatal(err)
	}
	if info.Number != 9 || info.Checks != "✓ 1" || info.ReviewDecision != "APPROVED" {
		t.Fatalf("%+v", info)
	}
}

func TestViewByHeadPrefersOpen(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr list"):
			return []byte(`[
				{"number":1,"url":"https://github.com/o/r/pull/1","title":"old","state":"MERGED","isDraft":false,"reviewDecision":"","headRefOid":"a","headRefName":"b"},
				{"number":2,"url":"https://github.com/o/r/pull/2","title":"new","state":"OPEN","isDraft":false,"reviewDecision":"","headRefOid":"c","headRefName":"b"}
			]`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[]`), nil
		default:
			t.Fatalf("unexpected %v", args)
			return nil, nil
		}
	}
	info, err := ViewByHeadWith(context.Background(), run, "/tmp", "grok/discord/1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Number != 2 {
		t.Fatalf("want open PR #2 got %+v", info)
	}
}
