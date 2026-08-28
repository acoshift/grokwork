package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/inbox"
	"github.com/acoshift/grokwork/internal/reviewstore"
)

func TestTeamReviewMention(t *testing.T) {
	t.Parallel()
	if got := teamReviewMention(true, "999888777666555444", ""); got != "999888777666555444" {
		t.Fatalf("discord actor: %q", got)
	}
	if got := teamReviewMention(false, "999888777666555444", ""); got != "" {
		t.Fatalf("web unit must not mention: %q", got)
	}
	if got := teamReviewMention(true, "google:alice", ""); got != "" {
		t.Fatalf("google only: %q", got)
	}
	if got := teamReviewMention(true, "google:alice", "111222333444555666"); got != "111222333444555666" {
		t.Fatalf("linked: %q", got)
	}
}

func TestNotifyTeamReviewRequestedInboxesWebUnit(t *testing.T) {
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
	if len(items) != 1 || items[0].Kind != inbox.KindReviewRequested {
		t.Fatalf("%+v", items)
	}
	if !strings.Contains(items[0].Subject, "acme/app#9") {
		t.Fatalf("subject=%q", items[0].Subject)
	}
	if items[0].Body != "please" || items[0].UnitID != "w_unit1" || items[0].Project != "proj" {
		t.Fatalf("%+v", items[0])
	}
	if items[0].URL != "/prs/acme/app/9?project=proj" {
		t.Fatalf("url=%q", items[0].URL)
	}
}

func TestNotifyTeamReviewRequestedInboxesAndMentions(t *testing.T) {
	dir := t.TempDir()
	ib, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{inbox: ib}
	b.NotifyTeamReviewRequested(reviewstore.Request{
		Owner: "acme", Repo: "app", Number: 4, Project: "proj",
		ThreadID: "1350013711103823954", ReviewerID: "999888777666555444",
	})
	items, err := ib.List("999888777666555444")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want inbox even when Discord can mention: %+v", items)
	}
}

func TestNotifyTeamReviewRequestedLinkedDiscordInboxesCanonical(t *testing.T) {
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
	if len(items) != 1 {
		t.Fatalf("canonical inbox = %+v", items)
	}
	if n := ib.Count("999888777666555444"); n != 0 {
		t.Fatalf("alias file has %d items", n)
	}
}
