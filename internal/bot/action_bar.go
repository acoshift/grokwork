package bot

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Run action bar custom_id prefix (ignore foreign components).
const actionBarPrefix = "gd:"

// custom_id actions (gd:<action>:<threadID>). Discord limit is 100 chars.
const (
	actionCancel      = "cancel"
	actionContinue    = "continue"
	actionReset       = "reset"
	actionResetOK     = "resetok"
	actionResetNo     = "resetno"
	actionHistory     = "history"
	actionContinueMod = "contmod" // modal submit custom_id
)

const (
	continueModalPromptID = "prompt"
	maxContinuePrompt     = 1800
)

func actionCustomID(action, threadID string) string {
	return actionBarPrefix + action + ":" + threadID
}

// parseActionCustomID returns action + threadID when id is ours.
func parseActionCustomID(id string) (action, threadID string, ok bool) {
	if !strings.HasPrefix(id, actionBarPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(id, actionBarPrefix)
	action, threadID, found := strings.Cut(rest, ":")
	if !found || action == "" || threadID == "" {
		return "", "", false
	}
	// Thread snowflakes are digits; keep flexible for tests.
	if strings.Contains(threadID, ":") {
		return "", "", false
	}
	return action, threadID, true
}

// webSessionURL builds the absolute admin UI URL for a work unit session page.
// publicBaseURL is config webPublicBaseURL (or GROK_WORK_PUBLIC_BASE_URL); empty → "".
// project is optional (?project= scopes the project-first shell).
func webSessionURL(threadID, publicBaseURL, project string) string {
	threadID = strings.TrimSpace(threadID)
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if threadID == "" || base == "" {
		return ""
	}
	u := base + "/sessions/" + url.PathEscape(threadID)
	if p := strings.TrimSpace(project); p != "" {
		u += "?project=" + url.QueryEscape(p)
	}
	return u
}

// sessionWebURL returns the absolute web session URL for a thread when configured.
func (b *Bot) sessionWebURL(threadID string) string {
	if b == nil {
		return ""
	}
	base := ""
	if b.cfg != nil {
		base = b.cfg.WebPublicBaseURLValue()
	}
	project := ""
	if b.sessions != nil {
		if e, ok := b.sessions.Get(threadID); ok {
			project = e.Project
		}
	}
	return webSessionURL(threadID, base, project)
}

// withWebSessionLine appends a clickable session URL so users can continue on the web UI.
func withWebSessionLine(content, sessionURL string) string {
	sessionURL = strings.TrimSpace(sessionURL)
	if sessionURL == "" {
		return content
	}
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return "Continue on web: " + sessionURL
	}
	return content + "\nContinue on web: " + sessionURL
}

// webLinkButton is a Discord link button to the web session page (nil components when url empty).
func webLinkButton(sessionURL string) (discordgo.MessageComponent, bool) {
	sessionURL = strings.TrimSpace(sessionURL)
	if sessionURL == "" {
		return nil, false
	}
	return discordgo.Button{
		Label: "Open on Web",
		Style: discordgo.LinkButton,
		URL:   sessionURL,
	}, true
}

// actionBarRunning is shown on the live status message while Grok is working.
// webURL is optional; when set, adds a link button to the web session page.
func actionBarRunning(threadID, webURL string) []discordgo.MessageComponent {
	btns := []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Cancel",
			Style:    discordgo.DangerButton,
			CustomID: actionCustomID(actionCancel, threadID),
		},
	}
	if link, ok := webLinkButton(webURL); ok {
		btns = append(btns, link)
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: btns},
	}
}

// actionBarDone is shown on the status message after a run finishes (or on /status).
// webURL is optional; when set, adds a link button to the web session page.
func actionBarDone(threadID, webURL string) []discordgo.MessageComponent {
	btns := []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Continue",
			Style:    discordgo.PrimaryButton,
			CustomID: actionCustomID(actionContinue, threadID),
		},
		discordgo.Button{
			Label:    "Reset",
			Style:    discordgo.SecondaryButton,
			CustomID: actionCustomID(actionReset, threadID),
		},
		discordgo.Button{
			Label:    "History",
			Style:    discordgo.SecondaryButton,
			CustomID: actionCustomID(actionHistory, threadID),
		},
	}
	if link, ok := webLinkButton(webURL); ok {
		btns = append(btns, link)
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: btns},
	}
}

func actionBarResetConfirm(threadID string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "Yes, reset",
					Style:    discordgo.DangerButton,
					CustomID: actionCustomID(actionResetOK, threadID),
				},
				discordgo.Button{
					Label:    "Never mind",
					Style:    discordgo.SecondaryButton,
					CustomID: actionCustomID(actionResetNo, threadID),
				},
			},
		},
	}
}

func continueModal(threadID string) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: actionCustomID(actionContinueMod, threadID),
			Title:    "Continue with Grok",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    continueModalPromptID,
							Label:       "Follow-up task",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "What should Grok do next?",
							Required:    true,
							MaxLength:   maxContinuePrompt,
							MinLength:   1,
						},
					},
				},
			},
		},
	}
}

func modalTextValue(data discordgo.ModalSubmitInteractionData, fieldID string) string {
	for _, row := range data.Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			ti, ok := c.(*discordgo.TextInput)
			if !ok {
				continue
			}
			if ti.CustomID == fieldID {
				return strings.TrimSpace(ti.Value)
			}
		}
	}
	return ""
}

// historyHint is the ephemeral reply for the History action-bar button.
// publicBaseURL is config webPublicBaseURL (or GROK_WORK_PUBLIC_BASE_URL); empty → path only.
func historyHint(threadID string, publicBaseURL string) string {
	path := "/history/" + threadID
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if base != "" {
		return fmt.Sprintf("Thread history (admin UI):\n%s%s", base, path)
	}
	return fmt.Sprintf("Thread history (admin UI, private network):\n`%s`", path)
}
