package deploy

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// discordMaxMsg mirrors internal/bot's cap.
const discordMaxMsg = 1900

// notifyLogLines is how much of a failed step's tail the notice carries.
const notifyLogLines = 20

// Notifier delivers a finished-deploy notice.
//
// Two sinks rather than one: a channel post reaches the team, but a web-only
// project has no channel, and a failed production deploy that notifies nobody
// is worse than a noisy one.
type Notifier interface {
	// SendChannel posts to a project's Discord channel. It must return an error
	// when the project has no channel configured, so the caller can fall back.
	SendChannel(project, content string) error
	// SendInbox appends to one actor's feed.
	SendInbox(actorID, subject, body, url, project string) error
}

// SetNotifier installs the delivery seam (main.go wires the bot's).
func (e *Engine) SetNotifier(n Notifier) { e.notifier = n }

// SetPublicBaseURL sets the base for run links in notices.
func (e *Engine) SetPublicBaseURL(u string) {
	e.publicBase = strings.TrimRight(strings.TrimSpace(u), "/")
}

// runURL returns a link to a run, or "" when no public base is configured.
func (e *Engine) runURL(r Run) string {
	if e.publicBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/projects/%s/deploys/%s", e.publicBase, r.Project, r.ID)
}

// notifyFinished posts one notice per terminal run.
//
// Never includes a filesystem path: local project paths may appear in the web
// UI (private network) but must never reach Discord.
func (e *Engine) notifyFinished(r Run) {
	if e.notifier == nil {
		return
	}
	content := e.formatNotice(r)
	if err := e.notifier.SendChannel(r.Project, content); err == nil {
		return
	} else {
		log.Printf("deploy: channel notice for %s: %v", r.ID, err)
	}
	// No channel (or delivery failed). A success needs no chasing, but a
	// non-success must reach the person who triggered it.
	if r.Status == StatusSucceeded || r.ActorID == "" {
		return
	}
	subject := fmt.Sprintf("Deploy %s → %s %s", r.Service, r.Env, r.Status)
	if err := e.notifier.SendInbox(r.ActorID, subject, content, e.runURL(r), r.Project); err != nil {
		log.Printf("deploy: inbox notice for %s: %v", r.ID, err)
	}
}

// formatNotice builds the Discord message for a finished run.
func (e *Engine) formatNotice(r Run) string {
	icon := "✅"
	headline := "deployed"
	switch r.Status {
	case StatusFailed:
		icon, headline = "❌", "failed"
	case StatusCancelled:
		icon, headline = "🚫", "cancelled"
	case StatusInterrupted:
		icon, headline = "⚠️", "interrupted"
	case StatusBlocked:
		icon, headline = "⚠️", "blocked"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s **%s → %s** %s", icon, r.Service, r.Env, headline)
	if failed := failedStep(r); failed != "" && r.Status == StatusFailed {
		fmt.Fprintf(&b, " at step `%s`", failed)
	}
	fmt.Fprintf(&b, " · `%s`", r.ShortSHA)
	if d := r.Elapsed(); d > 0 {
		fmt.Fprintf(&b, " · %s", d.Round(time.Second))
	}
	if r.ActorName != "" {
		fmt.Fprintf(&b, " · by %s", r.ActorName)
	}
	b.WriteString("\n")

	steps := len(r.Steps)
	fmt.Fprintf(&b, "%d step%s", steps, plural(steps))
	if u := e.runURL(r); u != "" {
		fmt.Fprintf(&b, " · <%s>", u)
	}
	if r.Status == StatusFailed {
		if tail := e.failedTail(r); tail != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```", tail)
		}
	}
	return clampDiscord(b.String())
}

func failedStep(r Run) string {
	for _, s := range r.Steps {
		if s.Status == StatusFailed {
			return s.Name
		}
	}
	return ""
}

// failedTail returns the last lines of the failed step's log.
//
// The log was written through the redactor, so secrets are already masked here;
// this reads what is on disk rather than re-deriving anything.
func (e *Engine) failedTail(r Run) string {
	for i, s := range r.Steps {
		if s.Status != StatusFailed {
			continue
		}
		chunk, _, err := e.store.ReadStepLogTail(r.ID, i, s.Name, 8<<10)
		if err != nil || len(chunk) == 0 {
			return ""
		}
		lines := strings.Split(strings.TrimRight(string(chunk), "\n"), "\n")
		if len(lines) > notifyLogLines {
			lines = lines[len(lines)-notifyLogLines:]
		}
		out := strings.Join(lines, "\n")
		// Leave room for the surrounding message and fences.
		if len(out) > 900 {
			out = "…" + out[len(out)-900:]
		}
		return out
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clampDiscord(s string) string {
	if len(s) <= discordMaxMsg {
		return s
	}
	return s[:discordMaxMsg-20] + "\n…(truncated)"
}
