package bot

import (
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestDiscordPRURLModes(t *testing.T) {
	gh := "https://github.com/acme/app/pull/9"
	b := &Bot{cfg: &config.Config{
		WebPublicBaseURL: "https://ui.example",
		DiscordPRLink:    config.DiscordPRLinkWeb,
	}}
	if got := b.discordPRURL("acme", "app", 9, gh); got != "https://ui.example/prs/acme/app/9" {
		t.Fatalf("web: %q", got)
	}
	info := b.withDiscordPRURL(ghpr.Info{Owner: "acme", Repo: "app", Number: 9, URL: gh, State: "OPEN"})
	if info.URL != "https://ui.example/prs/acme/app/9" {
		t.Fatalf("withDiscord: %q", info.URL)
	}
	// FormatCard must use display URL.
	card := ghpr.FormatCard(info)
	if !strings.Contains(card, "https://ui.example/prs/acme/app/9") {
		t.Fatalf("card missing web url:\n%s", card)
	}
	if strings.Contains(card, "github.com") {
		t.Fatalf("card still has github:\n%s", card)
	}

	b.cfg.DiscordPRLink = config.DiscordPRLinkGitHub
	if got := b.discordPRURL("acme", "app", 9, gh); got != gh {
		t.Fatalf("github: %q", got)
	}

	// Nil bot/cfg safe.
	var nilBot *Bot
	if got := nilBot.discordPRURL("a", "b", 1, gh); got != gh {
		t.Fatalf("nil bot: %q", got)
	}
}

func TestDiscordPRInfosRewrites(t *testing.T) {
	b := &Bot{cfg: &config.Config{
		WebPublicBaseURL: "http://127.0.0.1:8787",
		DiscordPRLink:    "web",
	}}
	e := sessionstore.Entry{}
	e.UpsertPR(sessionstore.TrackedPR{
		URL: "https://github.com/o/r/pull/3", Number: 3, State: "OPEN",
		Owner: "o", Repo: "r",
	})
	infos := b.discordPRInfos(e)
	if len(infos) != 1 || infos[0].URL != "http://127.0.0.1:8787/prs/o/r/3" {
		t.Fatalf("infos=%+v", infos)
	}
	// entryPRInfos (internal storage path) stays on GitHub.
	raw := entryPRInfos(e)
	if len(raw) != 1 || raw[0].URL != "https://github.com/o/r/pull/3" {
		t.Fatalf("raw infos must stay github: %+v", raw)
	}
}
