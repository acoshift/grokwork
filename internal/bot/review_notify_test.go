package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

func TestTeamReviewNotifyMentionVsInbox(t *testing.T) {
	t.Parallel()
	// Discord actor + Discord thread → mention, no inbox.
	snow, inbox := teamReviewNotify("123456789012345678", "999888777666555444", "", false)
	if snow != "999888777666555444" || inbox {
		t.Fatalf("discord thread: snow=%q inbox=%v", snow, inbox)
	}
	// Web unit → inbox even for a Discord actor.
	snow, inbox = teamReviewNotify("w_abcdef", "999888777666555444", "", false)
	if snow != "" || !inbox {
		t.Fatalf("web unit: snow=%q inbox=%v", snow, inbox)
	}
	// Empty thread → inbox.
	snow, inbox = teamReviewNotify("", "999888777666555444", "", false)
	if snow != "" || !inbox {
		t.Fatalf("empty: snow=%q inbox=%v", snow, inbox)
	}
	// Google-canonical with no Discord subject → inbox.
	snow, inbox = teamReviewNotify("123456789012345678", "google:alice", "", false)
	if snow != "" || !inbox {
		t.Fatalf("google only: snow=%q inbox=%v", snow, inbox)
	}
	// Google-canonical with linked Discord subject → mention.
	snow, inbox = teamReviewNotify("123456789012345678", "google:alice", "111222333444555666", true)
	if snow != "111222333444555666" || inbox {
		t.Fatalf("linked: snow=%q inbox=%v", snow, inbox)
	}
}

func TestNotifyTeamReviewRequestedInboxForWebUnit(t *testing.T) {
	dir := t.TempDir()
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{inbox: ib}
	b.NotifyTeamReviewRequested(reviewstore.Request{
		Owner: "acme", Repo: "app", Number: 9, Project: "proj",
		ThreadID: "w_unit1", ReviewerID: "google:alice", Note: "please",
	})
	items, err := ib.List("google:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "review.requested" {
		t.Fatalf("%+v", items)
	}
	if !strings.Contains(items[0].Subject, "acme/app#9") {
		t.Fatalf("subject=%q", items[0].Subject)
	}
	if items[0].Body != "please" || items[0].UnitID != "w_unit1" || items[0].Project != "proj" {
		t.Fatalf("%+v", items[0])
	}
}

func TestNotifyTeamReviewRequestedMentionSkipsInbox(t *testing.T) {
	dir := t.TempDir()
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{inbox: ib}
	// Discord snowflake reviewer + Discord thread: mention path, no inbox.
	b.NotifyTeamReviewRequested(reviewstore.Request{
		Owner: "acme", Repo: "app", Number: 4, Project: "proj",
		ThreadID: "1350013711103823954", ReviewerID: "999888777666555444",
	})
	items, err := ib.List("999888777666555444")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("mention path must not inbox: %+v", items)
	}
}

func TestNotifyTeamReviewRequestedLinkedDiscordMentions(t *testing.T) {
	dir := t.TempDir()
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Link("999888777666555444", "google:alice", ""); err != nil {
		t.Fatal(err)
	}
	b := &Bot{inbox: ib}
	b.SetIdentity(ids)
	b.NotifyTeamReviewRequested(reviewstore.Request{
		Owner: "acme", Repo: "app", Number: 7, Project: "proj",
		ThreadID: "1350013711103823954", ReviewerID: "google:alice",
	})
	items, err := ib.List("google:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("linked Discord must mention, not inbox: %+v", items)
	}
}
