package bot

import (
	"errors"
	"log"
	"regexp"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/audit"
)

// Audit trail for the Discord command surface.
//
// The web UI records every mutation it performs (internal/web auditAction), so
// until this existed the *primary* UX wrote nothing at all: an operator asking
// "who reset that thread" could only answer it for work done in the browser.
// Events here use the same audit.Event shape as web — actor, ok/error, detail
// map — so one reader consumes both surfaces, with detail["source"] naming which
// one produced the row.
//
// Two things never reach this log, both for the same reason the bot refuses to
// put them in Discord: message content / prompts (a task or a customer update
// can carry customer data) and local filesystem paths (project checkouts are
// private infrastructure). Ids, branch names, resolutions, counts and error
// strings are enough to answer "who did what to which unit", which is the whole
// job. Free-text command arguments — a checkpoint label, a close note — are
// deliberately dropped rather than trimmed, since "short" is not "safe".

// Denial reasons. Sentinel errors rather than ad-hoc strings so a reader can
// select every refused command with one match, and so the Discord reply wording
// (which changes for UX reasons) and the audit record cannot drift apart.
var (
	errAuditDeniedControl    = errors.New("forbidden: not thread owner, co-owner, or project admin")
	errAuditDeniedCapability = errors.New("forbidden: missing capability for this project")
	errAuditDeniedProject    = errors.New("forbidden: not a member of this project")
	errAuditDeniedQueueItem  = errors.New("forbidden: not the queue item author, thread owner, or project admin")
)

// auditCmd appends one Discord-originated event. Nil bot / nil logger is a
// no-op, so a Bot built without a data dir (most unit tests) needs no stub.
//
// err nil means the mutation succeeded; anything else is recorded verbatim as
// Event.Error with OK=false — including denials, which is the point: a refused
// /reset is exactly what an operator goes looking for.
func (b *Bot) auditCmd(action string, actor Actor, threadID, project string, err error, detail map[string]any) {
	if b == nil || b.audit == nil {
		return
	}
	d := make(map[string]any, len(detail)+3)
	for k, v := range detail {
		// Empty values are dropped rather than written: "previousOwnerId": "" reads
		// as a fact about the previous owner when it only means the caller had
		// nothing to say. Booleans and zero counts are kept — false and 0 are answers.
		if v == nil || v == "" {
			continue
		}
		d[k] = v
	}
	// Set last: a caller must not be able to mislabel the surface or retarget the
	// unit by passing these keys in detail.
	d["source"] = SourceDiscord
	if threadID != "" {
		d["threadId"] = threadID
	}
	if project != "" {
		d["project"] = project
	}
	for k, v := range d {
		switch t := v.(type) {
		case string:
			d[k] = scrubAuditPaths(t)
		case []string:
			cleaned := make([]string, len(t))
			for i, s := range t {
				cleaned[i] = scrubAuditPaths(s)
			}
			d[k] = cleaned
		}
	}
	ev := audit.Event{Action: action, Actor: actor.ID, Detail: d, OK: err == nil}
	if err != nil {
		ev.Error = scrubAuditPaths(err.Error())
	}
	if appendErr := b.audit.Append(ev); appendErr != nil {
		log.Printf("warn: audit %s thread=%s: %v", action, threadID, appendErr)
	}
}

// auditCmdMsg is auditCmd for the common case: actor and thread both come from
// the triggering message.
func (b *Bot) auditCmdMsg(action string, m *discordgo.MessageCreate, project string, err error, detail map[string]any) {
	if m == nil {
		return
	}
	b.auditCmd(action, ActorFromUser(m.Author), m.ChannelID, project, err, detail)
}

// auditPathChar is one character of a path: everything up to whitespace or a
// quote form, since those are what a message wraps a path in.
const auditPathChar = "[^\\s\"'`)]"

// auditPathRE matches absolute filesystem paths and managed worktree paths.
//
// Deliberately not anchored on a leading delimiter (unlike reAbsUnixPath in
// customer_sanitize.go): the text this guards is mostly git/gh stderr, where a
// path arrives glued to a colon — "fatal: not a git repository:/Users/…".
var auditPathRE = regexp.MustCompile(`(?i)(?:` +
	`[A-Za-z]:\\` + auditPathChar + `+` + // C:\Users\…
	`|/(?:Users|home|var|tmp|private|opt|usr|Volumes|Applications|Library)/` + auditPathChar + `*` +
	`|data/worktrees/` + auditPathChar + `*)`)

// scrubAuditPaths removes filesystem paths from text bound for the audit log.
//
// Applied centrally rather than per call site because most strings that reach
// here are subprocess stderr — git, gh, a project's verify command — whose
// wording is nobody's to control, and one forgotten call site is a permanent path
// leak into a file that is kept.
func scrubAuditPaths(s string) string {
	if s == "" {
		return s
	}
	return auditPathRE.ReplaceAllString(s, "[path]")
}
