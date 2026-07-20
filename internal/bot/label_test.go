package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestParseLabelArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/label", ""},
		{"label", ""},
		{"/label blocked", "blocked"},
		{"/label auto", "auto"},
		{"/label needs_review", "needs_review"},
	}
	for _, tc := range cases {
		if got := parseLabelArg(tc.in); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatBriefCardIncludesLabel(t *testing.T) {
	card := FormatBriefCard(BriefCardInput{
		Project:   "app",
		Goal:      "x",
		Label:     "needs review",
		LabelMode: "manual",
	})
	if !strings.Contains(card, "**label:** needs review (manual)") {
		t.Fatalf("%s", card)
	}
}

func TestFormatLabelStatus(t *testing.T) {
	e := sessionstore.Entry{Label: sessionstore.LabelBlocked, LabelManual: true}
	msg := formatLabelStatus(e)
	if !strings.Contains(msg, "blocked") || !strings.Contains(msg, "manual") {
		t.Fatalf("%s", msg)
	}
}
