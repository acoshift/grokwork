package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/ghpr"
)

func TestBuildFixCIPrompt(t *testing.T) {
	failed := []ghpr.Check{
		{Name: "test", Bucket: "fail", Link: "https://example.com/1"},
		{Name: "lint", Bucket: "fail"},
	}
	info := ghpr.Info{
		Number:  9,
		URL:     "https://github.com/o/r/pull/9",
		Owner:   "o",
		Repo:    "r",
		HeadSHA: "deadbeefcafebabe",
	}
	p := buildFixCIPrompt(info, "grok/discord/1", failed, "FAIL: boom")
	for _, want := range []string{
		"o/r#9", "deadbee", "grok/discord/1", "test", "lint",
		"gh pr checks", "do not merge", "FAIL: boom",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in prompt:\n%s", want, p)
		}
	}
}

func TestParseFixCI(t *testing.T) {
	for _, in := range []string{"/fix-ci", "fix-ci", "/fixci", "fixci"} {
		p := ParseMessage("<@1> "+in, "1")
		if p.Kind != KindFixCI {
			t.Fatalf("%q: got %+v", in, p)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abcdefghij"); got != "abcdefg" {
		t.Fatalf("got %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
