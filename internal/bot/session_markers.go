package bot

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/timeline"
)

// sessionLifecycleMarkerContract teaches agents the post-run lifecycle markers.
func sessionLifecycleMarkerContract() []string {
	return []string{
		"When this unit of work is finished, end your reply with exactly one of:",
		"  SESSION_DONE:",
		"  SESSION_ABANDON: <optional short reason>",
		"The host applies these after the run (do not invent admin UI HTTP calls).",
		"SESSION_DONE marks the board label done; SESSION_ABANDON soft-abandons (label + stop queue) without deleting your worktree.",
		"",
	}
}

// Session lifecycle markers in model output (post-run, both CLIs).
//
//	SESSION_DONE:
//	SESSION_ABANDON: optional short reason
// Markers must start at column 0. The prompt contract indents examples with two
// spaces so a model that quotes the contract does not soft-abandon the session.
var (
	sessionDoneRE    = regexp.MustCompile(`(?im)^SESSION_DONE:[ \t]*$`)
	sessionAbandonRE = regexp.MustCompile(`(?im)^SESSION_ABANDON:[ \t]*(.*)$`)
)

// sessionMarker is the host-side request parsed from a run reply.
type sessionMarker int

const (
	sessionMarkerNone sessionMarker = iota
	sessionMarkerDone
	sessionMarkerAbandon
)

// parseSessionLifecycleMarker returns the last lifecycle marker in text
// (greatest start index among all SESSION_DONE / SESSION_ABANDON matches).
func parseSessionLifecycleMarker(text string) (sessionMarker, string) {
	if text == "" {
		return sessionMarkerNone, ""
	}
	doneMatches := sessionDoneRE.FindAllStringIndex(text, -1)
	abMatches := sessionAbandonRE.FindAllStringSubmatchIndex(text, -1)
	if len(doneMatches) == 0 && len(abMatches) == 0 {
		return sessionMarkerNone, ""
	}
	lastDoneStart := -1
	if n := len(doneMatches); n > 0 {
		lastDoneStart = doneMatches[n-1][0]
	}
	lastAbStart := -1
	var lastAb []int
	if n := len(abMatches); n > 0 {
		lastAb = abMatches[n-1]
		lastAbStart = lastAb[0]
	}
	if lastDoneStart > lastAbStart {
		return sessionMarkerDone, ""
	}
	if lastAbStart < 0 {
		return sessionMarkerNone, ""
	}
	reason := ""
	if lastAb[2] >= 0 && lastAb[3] >= 0 {
		reason = strings.TrimSpace(text[lastAb[2]:lastAb[3]])
		// Clamp on runes: free-text is timeline-only, never audit body.
		if n := 0; len(reason) > 0 {
			for i := range reason {
				if n == 200 {
					reason = reason[:i]
					break
				}
				n++
			}
		}
	}
	return sessionMarkerAbandon, reason
}

// applySessionLifecycleMarkers applies SESSION_DONE / SESSION_ABANDON from replyText.
// Soft abandon never removes the worktree/branch (human ResetUnit / TTL does that).
// Returns the marker kind (for post-run gates such as direct-ship skip) and a short
// status note for Discord/UI when a marker was applied.
func (b *Bot) applySessionLifecycleMarkers(threadID, replyText, actor string) (sessionMarker, string) {
	if b == nil || strings.TrimSpace(threadID) == "" {
		return sessionMarkerNone, ""
	}
	kind, reason := parseSessionLifecycleMarker(replyText)
	switch kind {
	case sessionMarkerDone:
		return sessionMarkerDone, b.applySessionDoneMarker(threadID, actor)
	case sessionMarkerAbandon:
		return sessionMarkerAbandon, b.applySessionAbandonMarker(threadID, actor, reason)
	default:
		return sessionMarkerNone, ""
	}
}

func (b *Bot) applySessionDoneMarker(threadID, actor string) string {
	err := b.SetSessionLabel(threadID, sessionstore.LabelDone)
	b.auditAgentMarker(audit.ActionAgentSessionDone, threadID, actor, err, map[string]any{
		"source": "agent-marker",
		"label":  sessionstore.LabelDone,
	})
	if err != nil {
		log.Printf("session marker: done thread=%s: %v", threadID, err)
		return ""
	}
	b.appendTimeline(threadID, timeline.KindNotice, timeline.Notice{
		Level: "info",
		Text:  "Agent marked session done (SESSION_DONE).",
	})
	return "Session marked done by agent."
}

// SoftAbandonSession is the agent-safe abandon path: manual abandoned label,
// clear queue, cancel active run if any. It never removes worktree or branch.
func (b *Bot) SoftAbandonSession(threadID, who string) (msg string, err error) {
	if b == nil || b.sessions == nil || strings.TrimSpace(threadID) == "" {
		return "", fmt.Errorf("no session store")
	}
	var setErr error
	_, ok, patchErr := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		setErr = ent.SetLabelManual(sessionstore.LabelAbandoned)
		if setErr == nil {
			ent.StampTurn(ent.LastUser)
		}
	})
	if patchErr != nil {
		return "", patchErr
	}
	if !ok {
		return "", fmt.Errorf("no session for thread %s", threadID)
	}
	if setErr != nil {
		return "", setErr
	}
	n := b.clearQueue(threadID)
	if _, busy := b.getJob(threadID); busy {
		b.cancelCurrentRun(threadID, who)
	}
	msg = "Session abandoned by agent."
	if n > 0 {
		msg = fmt.Sprintf("Session abandoned by agent (cleared %d queued follow-up%s).", n, plural(n))
	}
	return msg, nil
}

func (b *Bot) applySessionAbandonMarker(threadID, actor, reason string) string {
	who := strings.TrimSpace(actor)
	if who == "" {
		who = "agent"
	}
	msg, err := b.SoftAbandonSession(threadID, who)
	detail := map[string]any{
		"source": "agent-marker",
		"label":  sessionstore.LabelAbandoned,
	}
	if reason != "" {
		// Timeline only — audit must not carry free-text task content; reason is short.
		detail["hasReason"] = true
	}
	b.auditAgentMarker(audit.ActionAgentSessionAbandon, threadID, actor, err, detail)
	if err != nil {
		log.Printf("session marker: abandon thread=%s: %v", threadID, err)
		return ""
	}
	note := "Agent abandoned session (SESSION_ABANDON)."
	if reason != "" {
		note += " Reason: " + reason
	}
	b.appendTimeline(threadID, timeline.KindNotice, timeline.Notice{Level: "info", Text: note})
	// Keep cwd/worktree: SoftAbandon must not call ResetUnit.
	if e, ok := b.sessions.Get(threadID); ok {
		if e.Cwd == "" && e.WorktreeBranch == "" {
			// Unexpected empty — still ok if never had a worktree.
		}
	}
	return msg
}

func (b *Bot) auditAgentMarker(action, threadID, actor string, err error, detail map[string]any) {
	if b == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["threadId"] = threadID
	if e, ok := b.sessions.Get(threadID); ok && e.Project != "" {
		detail["project"] = e.Project
	}
	// Prefer bot audit logger when present (Discord + web share patterns).
	if b.audit != nil {
		ev := audit.Event{
			Action: action,
			Actor:  strings.TrimSpace(actor),
			Detail: detail,
			OK:     err == nil,
		}
		if err != nil {
			ev.Error = err.Error()
		}
		if ev.Actor == "" {
			ev.Actor = audit.ActorAnonymous
		}
		_ = b.audit.Append(ev)
	}
}
