package bot

import (
	"fmt"
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

// actionBarRunning is shown on the live status message while Grok is working.
func actionBarRunning(threadID string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "Cancel",
					Style:    discordgo.DangerButton,
					CustomID: actionCustomID(actionCancel, threadID),
				},
			},
		},
	}
}

// actionBarDone is shown on the status message after a run finishes (or on /status).
func actionBarDone(threadID string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
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
			},
		},
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

func historyHint(threadID string, listenAddr string) string {
	path := "/history/" + threadID
	base := publicHistoryBase(listenAddr)
	if base != "" {
		return fmt.Sprintf("Thread history (admin UI):\n%s%s", base, path)
	}
	return fmt.Sprintf("Thread history (admin UI, private network):\n`%s`", path)
}

// publicHistoryBase turns a listen addr into an http origin when safe for a clickable URL.
func publicHistoryBase(listenAddr string) string {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return ""
	}
	host, port, ok := splitHostPortLoose(addr)
	if !ok {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	host = strings.Trim(host, "[]")
	if port == "" {
		return ""
	}
	return "http://" + host + ":" + port
}

func splitHostPortLoose(addr string) (host, port string, ok bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", false
	}
	// :8787
	if strings.HasPrefix(addr, ":") {
		return "", strings.TrimPrefix(addr, ":"), true
	}
	// [ipv6]:port
	if strings.HasPrefix(addr, "[") {
		end := strings.LastIndex(addr, "]:")
		if end < 0 {
			return "", "", false
		}
		return addr[1:end], addr[end+2:], true
	}
	// host:port
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}
