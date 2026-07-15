package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestParseMessage(t *testing.T) {
	p := ParseMessage("<@123> project:app fix bug", "123")
	if p.Kind != KindTask || p.Prompt != "project:app fix bug" {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> in api why timeout", "123")
	if p.Kind != KindTask || p.Prompt != "in api why timeout" {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> /help", "123")
	if p.Kind != KindHelp {
		t.Fatalf("got %+v", p)
	}

	p = ParseMessage("<@123> investigate timeout", "123")
	if p.Kind != KindTask || p.Prompt != "investigate timeout" {
		t.Fatalf("got %+v", p)
	}

	for _, in := range []string{"/cancel", "cancel", "/stop", "stop"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindCancel {
			t.Fatalf("%q: got %+v want KindCancel", in, p)
		}
	}
}

func TestParseMessagePreservesSpecialChars(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"<@123> fix issue #42", "fix issue #42"},
		{"<@123> see https://ex.com/path?foo=1&bar=2", "see https://ex.com/path?foo=1&bar=2"},
		{"<@123> check https://github.com/org/repo/issues/99#issuecomment-1", "check https://github.com/org/repo/issues/99#issuecomment-1"},
		{"<@123> org/repo#123 please", "org/repo#123 please"},
		{"<@123> a=1&b=2 still here", "a=1&b=2 still here"},
	}
	for _, tc := range cases {
		p := ParseMessage(tc.in, "123")
		if p.Kind != KindTask || p.Prompt != tc.want {
			t.Fatalf("in %q: got kind=%v prompt=%q want %q", tc.in, p.Kind, p.Prompt, tc.want)
		}
	}
}

func TestMessagePromptTextIncludesEmbedURL(t *testing.T) {
	m := &discordgo.Message{
		Content: "",
		Embeds: []*discordgo.MessageEmbed{
			{URL: "https://ex.com/a?x=1&y=2", Title: "Example"},
		},
	}
	got := messagePromptText(m)
	if !strings.Contains(got, "https://ex.com/a?x=1&y=2") {
		t.Fatalf("missing embed url: %q", got)
	}
	if !strings.Contains(got, "Example") {
		t.Fatalf("missing title: %q", got)
	}
}

func TestSanitizeDiscordContentKeepsHashAndQuery(t *testing.T) {
	in := "See #42 and https://x.com?a=1&b=2"
	if got := sanitizeDiscordContent(in); got != in {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeDiscordContent("ok\x00#1"); got != "ok#1" {
		t.Fatalf("null strip: %q", got)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{2 * time.Second, "2s"},
		{65 * time.Second, "1m 5s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h 2m"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%v)=%q want %q", tc.d, got, tc.want)
		}
	}
}

func TestWorkingStatus(t *testing.T) {
	got := workingStatus("app", 0, "")
	if got != "Working in **app**… · `@Grok /cancel` to stop" {
		t.Fatalf("initial: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "")
	if got != "Working in **app**… · 45s elapsed · `@Grok /cancel` to stop" {
		t.Fatalf("elapsed: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "reading files")
	if !strings.Contains(got, "reading files") {
		t.Fatalf("activity: %q", got)
	}
}
