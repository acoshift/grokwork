package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// P0 routed 92 raw s.ChannelMessageSendReply calls through discordReply. The raw
// call uses Discord's default mention parsing and lets links unfurl; the strict
// wrapper does neither. Converting to the wrong one is invisible in review and
// silently stops the /review reviewer ping, so pin both payload shapes.

func TestReplyPayloadKeepsDiscordDefaults(t *testing.T) {
	ref := &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}
	got := replyPayload("<@42> please review", ref)

	if got.AllowedMentions != nil {
		t.Fatalf("replyPayload must leave AllowedMentions nil (Discord default parsing) so command replies can ping; got %+v", got.AllowedMentions)
	}
	if got.Flags&discordgo.MessageFlagsSuppressEmbeds != 0 {
		t.Error("replyPayload must not suppress embeds: command replies carry PR/issue URLs whose unfurl is part of the reply")
	}
	if got.Reference != ref {
		t.Error("reply must stay a reply (Reference dropped)")
	}
	if got.Content != "<@42> please review" {
		t.Errorf("content = %q, want it passed through verbatim", got.Content)
	}
}

func TestStrictReplyPayloadSuppressesPingsAndUnfurls(t *testing.T) {
	got := strictReplyPayload("@everyone <@42>", nil)

	if got.AllowedMentions == nil {
		t.Fatal("strictReplyPayload must set AllowedMentions")
	}
	if len(got.AllowedMentions.Parse) != 0 {
		t.Errorf("Parse = %v, want empty so model output cannot ping", got.AllowedMentions.Parse)
	}
	if got.Flags&discordgo.MessageFlagsSuppressEmbeds == 0 {
		t.Error("strictReplyPayload must suppress embeds")
	}
}

func TestReplyPayloadsSanitizeContent(t *testing.T) {
	// Raw ChannelMessageSendReply did not sanitize; the wrappers do. A
	// whitespace-only reply would 400 unsanitized, so this is the one accepted
	// behavior change in the conversion.
	for name, got := range map[string]*discordgo.MessageSend{
		"reply":  replyPayload("a\x00b", nil),
		"strict": strictReplyPayload("a\x00b", nil),
	} {
		if strings.Contains(got.Content, "\x00") {
			t.Errorf("%s: NUL survived sanitize: %q", name, got.Content)
		}
	}
	for name, got := range map[string]*discordgo.MessageSend{
		"reply":  replyPayload("   ", nil),
		"strict": strictReplyPayload("   ", nil),
	} {
		if got.Content != "(empty response)" {
			t.Errorf("%s: whitespace-only content = %q, want the placeholder", name, got.Content)
		}
	}
}

// TestReviewRequestReplyCanPing guards the specific regression P0 nearly shipped:
// formatReviewRequestReply exists to notify the reviewer, so its delivery path
// must not be the strict payload.
func TestReviewRequestReplyCanPing(t *testing.T) {
	msg := formatReviewRequestReply(reviewRequestReply{
		ReviewerID:  "555",
		RequesterID: "111",
		Owner:       "o",
		Repo:        "r",
		Number:      7,
	})
	if !strings.Contains(msg, "<@555>") {
		t.Fatalf("review reply lost the reviewer mention: %q", msg)
	}
	if p := replyPayload(msg, nil); p.AllowedMentions != nil {
		t.Error("review reply must go out with default mention parsing or the reviewer is never notified")
	}
}
