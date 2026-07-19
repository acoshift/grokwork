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

	for _, in := range []string{"/fix-ci", "fix-ci"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindFixCI {
			t.Fatalf("%q: got %+v want KindFixCI", in, p)
		}
	}

	for _, in := range []string{"/claim", "claim"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindClaim {
			t.Fatalf("%q: got %+v want KindClaim", in, p)
		}
	}

	for _, in := range []string{"/hand-off <@999>", "/handoff <@999>", "/hand-off", "hand-off", "handoff"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindHandOff {
			t.Fatalf("%q: got %+v want KindHandOff", in, p)
		}
	}
	// Free-form text that starts with "hand-off " (no slash) is a normal task.
	p = ParseMessage("<@123> hand-off notes for later", "123")
	if p.Kind != KindTask {
		t.Fatalf("hand-off notes… should be task, got %+v", p)
	}

	for _, in := range []string{"/brief", "brief"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindBrief {
			t.Fatalf("%q: got %+v want KindBrief", in, p)
		}
	}
	p = ParseMessage("<@123> /brief goal ship auth", "123")
	if p.Kind != KindBrief || !strings.Contains(p.Prompt, "goal ship auth") {
		t.Fatalf("brief goal: got %+v", p)
	}
	// Bare "brief goal …" (no slash) is still the command.
	p = ParseMessage("<@123> brief goal ship auth", "123")
	if p.Kind != KindBrief {
		t.Fatalf("brief goal without slash: got %+v", p)
	}
	// Free-form "brief notes…" without slash stays a task.
	p = ParseMessage("<@123> brief notes for the team", "123")
	if p.Kind != KindTask {
		t.Fatalf("brief notes… should be task, got %+v", p)
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

func TestUnwrapAndExtractURLs(t *testing.T) {
	in := "check <https://backoffice.example.com/report/player?prefix=home1&providers=imagine&from=2026-07-15&to=2026-07-15&tz=7> please"
	got := unwrapDiscordLinks(in)
	if strings.Contains(got, "<https") {
		t.Fatalf("still wrapped: %q", got)
	}
	urls := extractURLs(in)
	if len(urls) != 1 {
		t.Fatalf("urls=%v", urls)
	}
	if !strings.Contains(urls[0], "prefix=home1") || !strings.Contains(urls[0], "tz=7") {
		t.Fatalf("query lost: %q", urls[0])
	}
	if !strings.Contains(urls[0], "https://backoffice.example.com/report/player?") {
		t.Fatalf("host/path lost: %q", urls[0])
	}
}

func TestEnrichPromptWithLinksBareURL(t *testing.T) {
	u := "https://ex.com/a?x=1&y=2#frag"
	got := enrichPromptWithLinks(u)
	if !strings.Contains(got, u) {
		t.Fatalf("missing url: %q", got)
	}
	if !strings.Contains(got, "analyze this link") && !strings.Contains(strings.ToLower(got), "url") {
		t.Fatalf("expected link guidance: %q", got)
	}
	// Angle-bracket form from Discord clients
	got = enrichPromptWithLinks("<" + u + ">")
	if strings.Contains(got, "<https") {
		t.Fatalf("should unwrap: %q", got)
	}
	if !strings.Contains(got, "x=1&y=2") {
		t.Fatalf("query lost: %q", got)
	}
}

func TestParseMessageAngleBracketLink(t *testing.T) {
	p := ParseMessage("<@123> see <https://ex.com/path?a=1&b=2#x>", "123")
	if p.Kind != KindTask {
		t.Fatalf("kind=%v", p.Kind)
	}
	// ParseMessage path uses message content before enrich; unwrap happens in messagePromptText/enrich.
	if !strings.Contains(p.Prompt, "https://ex.com/path?a=1&b=2") && !strings.Contains(p.Prompt, "<https://ex.com/path?a=1&b=2") {
		t.Fatalf("prompt=%q", p.Prompt)
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
	got := workingStatus("app", 0, "", "")
	if got != "Working in **app**… · `@Grok /cancel` to stop" {
		t.Fatalf("initial: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "", "")
	if got != "Working in **app**… · 45s elapsed · `@Grok /cancel` to stop" {
		t.Fatalf("elapsed: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "reading files", "read → edit → test → PR")
	if !strings.Contains(got, "reading files") {
		t.Fatalf("activity: %q", got)
	}
	if !strings.Contains(got, "read → edit → test → PR") {
		t.Fatalf("phases: %q", got)
	}
	// Phases line sits above the activity italic line.
	if i, j := strings.Index(got, "read →"), strings.Index(got, "_reading"); i < 0 || j < 0 || i > j {
		t.Fatalf("expected phases above activity: %q", got)
	}
}
