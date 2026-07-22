package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestHelpTextFitsDiscordChunks(t *testing.T) {
	// Discord content max is 2000; we reserve headroom via maxMsg and the multi-part prefix.
	const discordMax = 2000
	parts := splitMessage(HelpText())
	if len(parts) == 0 {
		t.Fatal("expected at least one part")
	}
	for i, p := range parts {
		content := p
		if len(parts) > 1 {
			content = fmt.Sprintf("(%d/%d)\n%s", i+1, len(parts), p)
		}
		if n := len([]rune(content)); n > discordMax {
			t.Fatalf("help chunk %d/%d is %d runes (limit %d)", i+1, len(parts), n, discordMax)
		}
		if len(content) > discordMax {
			t.Fatalf("help chunk %d/%d is %d bytes (limit %d)", i+1, len(parts), len(content), discordMax)
		}
	}
}

func TestParseStartAndQueueCommands(t *testing.T) {
	p := ParseMessage("<@1> /start investigate why timeout", "1")
	if p.Kind != KindStartInvestigate || !strings.Contains(p.Prompt, "timeout") {
		t.Fatalf("got kind=%d prompt=%q", p.Kind, p.Prompt)
	}
	p = ParseMessage("<@1> /investigate flaky test", "1")
	if p.Kind != KindStartInvestigate {
		t.Fatalf("kind=%d", p.Kind)
	}
	p = ParseMessage("<@1> /queue", "1")
	if p.Kind != KindQueue {
		t.Fatalf("kind=%d", p.Kind)
	}
	p = ParseMessage("<@1> /dequeue 2", "1")
	if p.Kind != KindDequeue || strings.TrimSpace(p.Arg) != "2" {
		t.Fatalf("kind=%d arg=%q", p.Kind, p.Arg)
	}
	p = ParseMessage("<@1> /cancel-mine", "1")
	if p.Kind != KindCancelMine {
		t.Fatalf("kind=%d", p.Kind)
	}
	// freeform "investigate timeout" stays a task
	p = ParseMessage("<@1> investigate timeout", "1")
	if p.Kind != KindTask {
		t.Fatalf("freeform investigate should be task, kind=%d", p.Kind)
	}
}

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

	for _, in := range []string{"/label", "label"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindLabel {
			t.Fatalf("%q: got %+v want KindLabel", in, p)
		}
	}
	p = ParseMessage("<@123> /label blocked", "123")
	if p.Kind != KindLabel || !strings.Contains(p.Prompt, "blocked") {
		t.Fatalf("label blocked: got %+v", p)
	}
	// Bare "label notes…" without slash stays a task.
	p = ParseMessage("<@123> label this carefully in the UI", "123")
	if p.Kind != KindTask {
		t.Fatalf("label free-form should be task, got %+v", p)
	}

	for _, in := range []string{"/board", "board"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindBoard {
			t.Fatalf("%q: got %+v want KindBoard", in, p)
		}
	}
	p = ParseMessage("<@123> /board needs_review", "123")
	if p.Kind != KindBoard || !strings.Contains(p.Prompt, "needs_review") {
		t.Fatalf("board filter: got %+v", p)
	}
	p = ParseMessage("<@123> board the room with posters", "123")
	if p.Kind != KindTask {
		t.Fatalf("board free-form should be task, got %+v", p)
	}

	for _, in := range []string{"/link", "link", "/unlink", "unlink"} {
		p = ParseMessage("<@123> "+in, "123")
		if p.Kind != KindLink {
			t.Fatalf("%q: got %+v want KindLink", in, p)
		}
	}
	p = ParseMessage("<@123> /link #42", "123")
	if p.Kind != KindLink || !strings.Contains(p.Prompt, "#42") {
		t.Fatalf("link #42: got %+v", p)
	}
	p = ParseMessage("<@123> /unlink #42", "123")
	if p.Kind != KindLink {
		t.Fatalf("unlink: got %+v", p)
	}
	// Free-form "link the docs" without slash stays a task.
	p = ParseMessage("<@123> link the docs in the README", "123")
	if p.Kind != KindTask {
		t.Fatalf("link free-form should be task, got %+v", p)
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
	got := startingStatus("app")
	if got != "Starting · **app**…" {
		t.Fatalf("starting: %q", got)
	}
	got = workingStatus("app", 0, "", "")
	if got != "Working in **app**… · Cancel button or `@Grok /cancel`" {
		t.Fatalf("initial: %q", got)
	}
	got = workingStatus("app", 45*time.Second, "", "")
	if got != "Working in **app**… · 45s elapsed · Cancel button or `@Grok /cancel`" {
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
