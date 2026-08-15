package bot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/timeline"
)

const planIssueLabel = "plan"

var (
	planIssueStartRE    = regexp.MustCompile(`(?im)^PLAN_ISSUE:[ \t]*\n`)
	planIssueTitleRE    = regexp.MustCompile(`(?im)^title:[ \t]*(.+)$`)
	scrutinizeVerdictRE = regexp.MustCompile(`(?im)^SCRUTINIZE_VERDICT:[ \t]*(ship|fix-then-ship|rework|reject)\b`)
	// Column-0 host markers that close a PLAN_ISSUE body.
	planIssueEndRE = regexp.MustCompile(`(?im)^(?:SESSION_DONE:|SESSION_ABANDON:|DECISION:|SCRUTINIZE_VERDICT:|PLAN_ISSUE:)`)
)

type planIssueSpec struct {
	Title string
	Body  string
}

type planFileOutcome struct {
	Filed   bool
	Updated bool
	Note    string
	Issue   sessionstore.TrackedIssue
}

func parsePlanIssue(text string) (spec planIssueSpec, remainder string, ok bool) {
	remainder = text
	loc := planIssueStartRE.FindStringIndex(text)
	if loc == nil {
		return planIssueSpec{}, text, false
	}
	rest := text[loc[1]:]
	end := len(rest)
	if m := planIssueEndRE.FindStringIndex(rest); m != nil {
		end = m[0]
	}
	block := rest[:end]
	remainder = strings.TrimSpace(text[:loc[0]] + "\n" + rest[end:])

	title := ""
	body := block
	if tm := planIssueTitleRE.FindStringSubmatch(block); len(tm) == 2 {
		title = strings.TrimSpace(tm[1])
		// Drop the title line from the body.
		if i := strings.Index(block, tm[0]); i >= 0 {
			body = strings.TrimSpace(block[:i] + block[i+len(tm[0]):])
		}
	}
	title = clampPlanTitle(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" {
		return planIssueSpec{}, remainder, false
	}
	return planIssueSpec{Title: title, Body: body}, remainder, true
}

func clampPlanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "#")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= 256 {
		return s
	}
	r := []rune(s)
	return string(r[:256])
}

func parseScrutinizeVerdict(text string) string {
	ms := scrutinizeVerdictRE.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(ms[len(ms)-1][1]))
}

func planBodyHasTasklist(body string) bool {
	if strings.Contains(body, "<!-- grokwork:tasklist -->") {
		return true
	}
	return len(ParseTasklist(body)) > 0
}

func boundGitHubPlanIssue(e sessionstore.Entry) (sessionstore.TrackedIssue, bool) {
	for _, iss := range e.Issues {
		if iss.IsLinear() || iss.IsClickUp() {
			continue
		}
		if iss.Number > 0 {
			return iss, true
		}
	}
	return sessionstore.TrackedIssue{}, false
}

func hasOpenQuestions(e sessionstore.Entry) bool {
	for _, q := range e.OpenQuestions {
		if strings.EqualFold(strings.TrimSpace(q.Status), "open") {
			return true
		}
	}
	return false
}

// maybeFilePlanIssue files or updates the plan GitHub issue from a completed plan run.
// Call after OpenQuestions for this turn have been persisted.
func (b *Bot) maybeFilePlanIssue(threadID, project, cwd string, actor Actor, reply string, cancelled, maxTurns bool, exitCode int) planFileOutcome {
	if b == nil {
		return planFileOutcome{Note: "bot is nil"}
	}
	if cancelled {
		return planFileOutcome{Note: "run cancelled — plan issue not filed"}
	}
	if maxTurns {
		return planFileOutcome{Note: "max turns reached — plan issue not filed"}
	}
	if exitCode != 0 {
		return planFileOutcome{Note: fmt.Sprintf("run exit %d — plan issue not filed", exitCode)}
	}

	spec, remainder, ok := parsePlanIssue(reply)
	verdict := parseScrutinizeVerdict(remainder)
	if verdict == "" {
		verdict = parseScrutinizeVerdict(reply)
	}

	var ent sessionstore.Entry
	if b.sessions != nil {
		if e, found := b.sessions.Get(threadID); found {
			ent = e
		}
	}
	if hasOpenQuestions(ent) {
		return planFileOutcome{Note: "open questions remain — plan issue not filed"}
	}
	if verdict != "ship" {
		if verdict == "" {
			return planFileOutcome{Note: "missing SCRUTINIZE_VERDICT: ship — plan issue not filed"}
		}
		return planFileOutcome{Note: "SCRUTINIZE_VERDICT is " + verdict + " — plan issue not filed"}
	}
	if !ok {
		return planFileOutcome{Note: "missing PLAN_ISSUE block — plan issue not filed"}
	}
	if !planBodyHasTasklist(spec.Body) {
		return planFileOutcome{Note: "PLAN_ISSUE has no Breakdown tasklist — plan issue not filed"}
	}

	body := b.attributePlanIssueBody(threadID, actor, spec.Body)
	body = audit.ScrubPaths(body)

	if existing, found := boundGitHubPlanIssue(ent); found {
		return b.updatePlanIssue(threadID, project, cwd, actor, existing, spec.Title, body)
	}
	return b.createPlanIssue(threadID, project, cwd, actor, spec.Title, body)
}

func (b *Bot) attributePlanIssueBody(threadID string, actor Actor, body string) string {
	login := ""
	if actor.ID != "" {
		if l, _, ok := b.githubIdentityFor(actor.ID); ok {
			login = l
		}
	}
	out := OnBehalfOfCommentBody(actor.DisplayName, login, body)
	if url := b.sessionWebURL(threadID); url != "" {
		out = strings.TrimRight(out, "\n") + "\n\n---\nPlanned in Grok Work: " + url + "\n"
	}
	return out
}

func (b *Bot) createPlanIssue(threadID, project, cwd string, actor Actor, title, body string) planFileOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	n, url, err := ghpr.CreateIssueWith(ctx, b.ghRun(), cwd, "", "", ghpr.CreateIssueOpts{
		Title:  title,
		Body:   body,
		Labels: []string{planIssueLabel},
	})
	b.auditPlanIssue(actor, threadID, project, n, url, err, false)
	if err != nil {
		log.Printf("plan issue: create thread=%s: %v", threadID, err)
		return planFileOutcome{Note: "could not create plan issue: " + err.Error()}
	}
	iss := sessionstore.TrackedIssue{
		Number:  n,
		URL:     url,
		Title:   title,
		Keyword: sessionstore.IssueKeywordRefs,
	}
	iss.FillFromURL()
	b.bindPlanIssue(threadID, iss)
	note := "Opened plan issue"
	if iss.Owner != "" && iss.Repo != "" {
		note = fmt.Sprintf("Opened plan issue %s/%s#%d", iss.Owner, iss.Repo, n)
	} else if n > 0 {
		note = fmt.Sprintf("Opened plan issue #%d", n)
	}
	if url != "" {
		note += " — " + url
	}
	b.appendTimeline(threadID, timeline.KindNotice, timeline.Notice{Level: "info", Text: note})
	return planFileOutcome{Filed: true, Note: note, Issue: iss}
}

func (b *Bot) updatePlanIssue(threadID, project, cwd string, actor Actor, existing sessionstore.TrackedIssue, title, body string) planFileOutcome {
	if existing.Number <= 0 {
		return planFileOutcome{Note: "bound plan issue has no number — not updated"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := ghpr.EditIssueBodyWith(ctx, b.ghRun(), cwd, existing.Owner, existing.Repo, existing.Number, body)
	b.auditPlanIssue(actor, threadID, project, existing.Number, existing.URL, err, true)
	if err != nil {
		log.Printf("plan issue: edit thread=%s #%d: %v", threadID, existing.Number, err)
		return planFileOutcome{Note: "could not update plan issue: " + err.Error()}
	}
	_ = title // title stays; gh issue edit --title is out of v1
	note := "Updated plan issue"
	if existing.Owner != "" && existing.Repo != "" {
		note = fmt.Sprintf("Updated plan issue %s/%s#%d", existing.Owner, existing.Repo, existing.Number)
	} else {
		note = fmt.Sprintf("Updated plan issue #%d", existing.Number)
	}
	if existing.URL != "" {
		note += " — " + existing.URL
	}
	b.appendTimeline(threadID, timeline.KindNotice, timeline.Notice{Level: "info", Text: note})
	return planFileOutcome{Filed: true, Updated: true, Note: note, Issue: existing}
}

func (b *Bot) bindPlanIssue(threadID string, iss sessionstore.TrackedIssue) {
	if b == nil || b.sessions == nil || threadID == "" || iss.Number <= 0 {
		return
	}
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.UpsertIssueForceKeyword(iss)
	})
	if err != nil {
		log.Printf("plan issue: bind thread=%s: %v", threadID, err)
	}
}

func (b *Bot) auditPlanIssue(actor Actor, threadID, project string, number int, url string, err error, updated bool) {
	if b == nil || b.audit == nil {
		return
	}
	detail := map[string]any{
		"project":  project,
		"threadId": threadID,
		"kind":     "plan",
		"number":   number,
		"updated":  updated,
	}
	if url != "" {
		detail["url"] = url
	}
	b.auditCmd(audit.ActionIssueCreate, actor, threadID, project, err, detail)
}

// planSessionDoneAllowed reports whether a plan run may apply SESSION_DONE
// after filing (or when the issue was already bound and this turn met file preconditions).
func planSessionDoneAllowed(out planFileOutcome) bool {
	return out.Filed
}
