package bot

import (
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// discordPRURL returns the URL to show in Discord for a PR (GitHub or web UI per config).
func (b *Bot) discordPRURL(owner, repo string, number int, githubURL string) string {
	if b == nil || b.cfg == nil {
		if githubURL != "" {
			return githubURL
		}
		return ""
	}
	return b.cfg.DiscordPRDisplayURL(owner, repo, number, githubURL)
}

// withDiscordPRURL returns a copy of info with URL rewritten for Discord display.
// Session storage and gh selectors keep the original GitHub URL.
func (b *Bot) withDiscordPRURL(info ghpr.Info) ghpr.Info {
	info.URL = b.discordPRURL(info.Owner, info.Repo, info.Number, info.URL)
	return info
}

// discordPRInfos builds ghpr.Info rows for Discord (display URLs applied).
func (b *Bot) discordPRInfos(e sessionstore.Entry) []ghpr.Info {
	infos := entryPRInfos(e)
	for i := range infos {
		infos[i] = b.withDiscordPRURL(infos[i])
	}
	return infos
}
