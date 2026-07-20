package bot

import (
	"errors"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

var errNoEmbeds = errors.New("no embeds")

// Discord link forms we must preserve end-to-end:
//   https://host/path?a=1&b=2#frag
//   <https://host/path?a=1&b=2>   (client "suppress embed" wrap)
//   `https://…`                   (inline code)

var (
	// angleURL matches Discord's <https://...> / <http://...> suppressed-embed form.
	angleURLRE = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	// looseURL finds http(s) URLs including query/fragment characters Discord keeps.
	looseURLRE = regexp.MustCompile(`https?://[^\s<>\]]+`)
)

// unwrapDiscordLinks turns <https://...> into https://... so the model sees a normal URL.
func unwrapDiscordLinks(s string) string {
	return angleURLRE.ReplaceAllString(s, "$1")
}

func containsURL(s string) bool {
	return looseURLRE.FindString(s) != ""
}

func extractURLs(s string) []string {
	s = unwrapDiscordLinks(s)
	found := looseURLRE.FindAllString(s, -1)
	if len(found) == 0 {
		return nil
	}
	// Trim trailing markdown/punctuation commonly stuck to URLs.
	out := make([]string, 0, len(found))
	seen := map[string]struct{}{}
	for _, u := range found {
		u = strings.TrimRight(u, ".,);]!>*`\"'")
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// enrichPromptWithLinks normalizes Discord link markup and adds guidance when URLs are present.
func enrichPromptWithLinks(prompt string) string {
	prompt = unwrapDiscordLinks(prompt)
	urls := extractURLs(prompt)
	if len(urls) == 0 {
		return prompt
	}

	// Bare-link messages become an explicit analysis request.
	if isMostlyURL(prompt) {
		prompt = "Please analyze this link and related code/product behavior:\n" + strings.Join(urls, "\n")
	}

	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n")
	b.WriteString("URLs from the user (keep verbatim, including query params and #fragments):\n")
	for _, u := range urls {
		b.WriteString("- ")
		b.WriteString(u)
		b.WriteString("\n")
	}
	b.WriteString("If a URL is private/backoffice and not fetchable from this machine, investigate the matching routes/handlers in this repository and still give a concrete answer.\n")
	return b.String()
}

func isMostlyURL(s string) bool {
	s = strings.TrimSpace(unwrapDiscordLinks(s))
	if s == "" {
		return false
	}
	// Strip surrounding backticks.
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	urls := extractURLs(s)
	if len(urls) == 0 {
		return false
	}
	// Treat as bare-link when removing URLs leaves almost nothing.
	rest := s
	for _, u := range urls {
		rest = strings.ReplaceAll(rest, u, "")
	}
	rest = strings.TrimSpace(rest)
	rest = strings.Trim(rest, "`<>")
	return rest == "" || len(rest) < 8
}

// discordSend posts with embeds suppressed so streaming/edits of messages that
// contain URLs (and partial URLs mid-stream) do not thrash Discord's embed crawler
// or fail when Embed Links is missing / rate-limited.
func discordSend(s *discordgo.Session, channelID, content string) (*discordgo.Message, error) {
	return discordSendComponents(s, channelID, content, nil)
}

func discordSendComponents(s *discordgo.Session, channelID, content string, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	content = sanitizeDiscordContent(content)
	msg := &discordgo.MessageSend{
		Content: content,
		Flags:   discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			// Do not ping roles/everyone from model output.
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	if len(components) > 0 {
		msg.Components = components
	}
	return s.ChannelMessageSendComplex(channelID, msg)
}

func discordEdit(s *discordgo.Session, channelID, msgID, content string) error {
	return discordEditComponents(s, channelID, msgID, content, nil, false)
}

// discordEditComponents edits content and optionally replaces or clears components.
// When setComponents is false, existing buttons are left unchanged (field omitted).
func discordEditComponents(s *discordgo.Session, channelID, msgID, content string, components []discordgo.MessageComponent, setComponents bool) error {
	content = sanitizeDiscordContent(content)
	edit := &discordgo.MessageEdit{
		Channel: channelID,
		ID:      msgID,
		Content: &content,
		Flags:   discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	if setComponents {
		// Empty slice clears buttons; non-empty replaces the action row(s).
		comps := components
		if comps == nil {
			comps = []discordgo.MessageComponent{}
		}
		edit.Components = &comps
	}
	_, err := s.ChannelMessageEditComplex(edit)
	return err
}

func discordSendReply(s *discordgo.Session, channelID, content string, reference *discordgo.MessageReference) (*discordgo.Message, error) {
	content = sanitizeDiscordContent(content)
	return s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:   content,
		Reference: reference,
		Flags:     discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})
}

// discordSendEmbed posts a rich embed. Unlike discordSend, it does not set
// SuppressEmbeds — that flag would drop custom embeds as well as link unfurls.
func discordSendEmbed(s *discordgo.Session, channelID string, embeds ...*discordgo.MessageEmbed) (*discordgo.Message, error) {
	if len(embeds) == 0 {
		return nil, errNoEmbeds
	}
	cleaned := make([]*discordgo.MessageEmbed, 0, len(embeds))
	for _, e := range embeds {
		if e == nil {
			continue
		}
		// Strip NULs only; do not run full content sanitize (empty → placeholder).
		e.Title = strings.ReplaceAll(e.Title, "\x00", "")
		e.Description = strings.ReplaceAll(e.Description, "\x00", "")
		e.URL = strings.ReplaceAll(e.URL, "\x00", "")
		for _, f := range e.Fields {
			if f == nil {
				continue
			}
			f.Name = strings.ReplaceAll(f.Name, "\x00", "")
			f.Value = strings.ReplaceAll(f.Value, "\x00", "")
		}
		cleaned = append(cleaned, e)
	}
	if len(cleaned) == 0 {
		return nil, errNoEmbeds
	}
	return s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds: cleaned,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})
}

