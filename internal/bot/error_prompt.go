package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// BuildErrorFixPrompt is the Fix-from-error task body. The sample stack is not
// embedded — the agent re-gets it via MCP.
func BuildErrorFixPrompt(actorDisplay string, tracked sessionstore.TrackedError, direct bool) string {
	return buildErrorPrompt(actorDisplay, tracked, true, direct)
}

// BuildErrorInvestigatePrompt is the Investigate-from-error task body (no PR contract).
func BuildErrorInvestigatePrompt(actorDisplay string, tracked sessionstore.TrackedError) string {
	return buildErrorPrompt(actorDisplay, tracked, false, false)
}

func buildErrorPrompt(actorDisplay string, tracked sessionstore.TrackedError, fix, direct bool) string {
	actorDisplay = strings.TrimSpace(actorDisplay)
	if actorDisplay == "" {
		actorDisplay = "web user"
	}
	display := tracked.DisplayRef()
	if display == "" {
		display = strings.TrimSpace(tracked.ID)
	}
	title := strings.TrimSpace(tracked.Title)
	provider := strings.TrimSpace(tracked.Provider)
	tool := errorMCPGetTool(provider)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task (started from web by %s)\n", actorDisplay)
	if fix {
		fmt.Fprintf(&b, "Fix production error %s: %s\n", display, title)
	} else {
		fmt.Fprintf(&b, "Investigate production error %s: %s\n", display, title)
	}
	fmt.Fprintf(&b, "Provider: %s\n", provider)
	if u := strings.TrimSpace(tracked.URL); u != "" {
		fmt.Fprintf(&b, "URL: %s\n", u)
	}
	meta := errorPromptMeta(tracked)
	if meta != "" {
		fmt.Fprintf(&b, "%s\n", meta)
	}
	fmt.Fprintf(&b, "\nUse grokwork MCP %s to load the sample stack. Do not call provider HTTP.\n", tool)
	b.WriteString("Do not invent tokens. Do not resolve, mute, or assign.\n")
	b.WriteString("Do not paste full stacks, request payloads, or PII into Discord — summarize.\n")
	if fix {
		b.WriteString(fixPromptShipSteps(direct, ""))
	}
	return b.String()
}

func errorMCPGetTool(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case sessionstore.ErrorProviderSentry:
		return "sentry_get_issue"
	case sessionstore.ErrorProviderGCP:
		return "gcp_errors_get"
	default:
		return "deploys_errors_get"
	}
}

func errorPromptMeta(tracked sessionstore.TrackedError) string {
	var parts []string
	if s := strings.TrimSpace(tracked.Status); s != "" {
		parts = append(parts, s)
	}
	if tracked.Count > 0 {
		parts = append(parts, fmt.Sprintf("%d×", tracked.Count))
	}
	if ls := strings.TrimSpace(tracked.LastSeen); ls != "" {
		if t, err := time.Parse(time.RFC3339, ls); err == nil {
			parts = append(parts, "last "+t.UTC().Format(time.RFC3339))
		} else {
			parts = append(parts, "last "+ls)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Status: " + strings.Join(parts, " · ")
}

func errorGoal(tracked sessionstore.TrackedError) string {
	title := strings.TrimSpace(tracked.Title)
	id := strings.TrimSpace(tracked.ID)
	switch strings.ToLower(strings.TrimSpace(tracked.Provider)) {
	case sessionstore.ErrorProviderSentry:
		ref := tracked.DisplayRef()
		if title != "" {
			return "Sentry " + ref + ": " + title
		}
		return "Sentry " + ref
	case sessionstore.ErrorProviderGCP:
		if title != "" {
			return "GCP " + id + ": " + title
		}
		return "GCP " + id
	default:
		if title != "" {
			return "deploys " + id + ": " + title
		}
		return "deploys " + id
	}
}
