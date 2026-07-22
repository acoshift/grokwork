package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// preserveIssueFields copies bound issues when session Set overwrites the entry.
func preserveIssueFields(next *sessionstore.Entry, prev sessionstore.Entry) {
	if next == nil {
		return
	}
	if len(next.Issues) == 0 && len(prev.Issues) > 0 {
		next.Issues = append([]sessionstore.TrackedIssue(nil), prev.Issues...)
	}
}

// issueBindingPrompt injects linked-ticket contract for Grok (PR body + title).
func issueBindingPrompt(issues []sessionstore.TrackedIssue) string {
	return issueBindingPromptMode(issues, false)
}

// issueBindingPromptMode is like issueBindingPrompt; direct=true puts Fixes/Refs
// in commit messages instead of PR body (No-PR ship mode).
func issueBindingPromptMode(issues []sessionstore.TrackedIssue, direct bool) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Linked tickets for this Discord thread:\n")
	for _, iss := range issues {
		ref := iss.DisplayRef()
		b.WriteString(fmt.Sprintf("- %s (%s)", ref, iss.EffectiveKeyword()))
		if iss.IsLinear() {
			if st := strings.TrimSpace(iss.State); st != "" {
				b.WriteString(" · " + st)
			}
			if t := strings.TrimSpace(iss.Title); t != "" {
				b.WriteString("\n  Title: " + truncateRunes(t, 200))
			}
		}
		if u := strings.TrimSpace(iss.URL); u != "" {
			b.WriteString(" · " + u)
		}
		b.WriteString("\n")
	}
	if direct {
		b.WriteString("This thread ships direct-to-primary (no PR). When you commit you MUST:\n")
		b.WriteString("1. Include these exact lines in the commit message body:\n")
		for _, iss := range issues {
			if line := iss.PRBodyLine(); line != "" {
				b.WriteString("   " + line + "\n")
			}
		}
		b.WriteString("2. Prefer a short summary that mentions the ticket ids when relevant (e.g. \"")
		b.WriteString(strings.TrimSpace(sessionstore.IssueTitlePrefix(issues)))
		b.WriteString(" short summary\").\n")
		b.WriteString("Do not invent other issue numbers. Do not open a PR for this project's repository.\n\n")
		return b.String()
	}
	b.WriteString("When you open or update a pull request you MUST:\n")
	b.WriteString("1. Include these exact lines in the PR body:\n")
	for _, iss := range issues {
		if line := iss.PRBodyLine(); line != "" {
			b.WriteString("   " + line + "\n")
		}
	}
	b.WriteString("2. Prefix the PR title with the ticket ids if missing (e.g. \"")
	b.WriteString(strings.TrimSpace(sessionstore.IssueTitlePrefix(issues)))
	b.WriteString(" short summary\").\n")
	hasLinear := false
	for _, iss := range issues {
		if iss.IsLinear() {
			hasLinear = true
			break
		}
	}
	if hasLinear {
		b.WriteString("3. Prefer branch names containing the Linear identifier (lowercase, e.g. eng-123-…) when you choose a new branch name.\n")
		b.WriteString("Linear state is driven by its GitHub integration via the identifier in title/body — do not invent other ticket ids.\n")
	}
	b.WriteString("Do not invent other issue numbers. Do not merge the PR.\n\n")
	return b.String()
}

// bindIssuesFromText parses GitHub issue refs from text and upserts them onto the session.
// Returns bound issues after upsert (may be empty). defaultOwner/Repo fill bare #N.
func (b *Bot) bindIssuesFromText(threadID, text, defaultOwner, defaultRepo string) []sessionstore.TrackedIssue {
	if b == nil || b.sessions == nil || threadID == "" {
		return nil
	}
	parsed := sessionstore.ParseIssueRefs(text)
	if len(parsed) == 0 {
		return nil
	}
	sessionstore.FillIssueOwnerRepo(parsed, defaultOwner, defaultRepo)
	return b.upsertIssues(threadID, parsed)
}

// bindLinearIssuesFromText parses Linear refs when the project has Linear enabled.
// Optionally resolves via API when a project API key is set.
func (b *Bot) bindLinearIssuesFromText(threadID, project, text string) []sessionstore.TrackedIssue {
	if b == nil || b.sessions == nil || threadID == "" || text == "" {
		return nil
	}
	if b.cfg == nil || !b.cfg.ProjectLinearEnabled(project) {
		return nil
	}
	parsed := sessionstore.ParseLinearIssueRefs(text)
	if len(parsed) == 0 {
		return nil
	}
	b.resolveLinearIssues(project, parsed)
	return b.upsertIssues(threadID, parsed)
}

func (b *Bot) upsertIssues(threadID string, issues []sessionstore.TrackedIssue) []sessionstore.TrackedIssue {
	if len(issues) == 0 {
		return nil
	}
	var bound []sessionstore.TrackedIssue
	_, ok, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		for _, iss := range issues {
			ent.UpsertIssue(iss)
		}
		bound = append([]sessionstore.TrackedIssue(nil), ent.Issues...)
	})
	if err != nil {
		log.Printf("warn: bind issues thread=%s: %v", threadID, err)
		return nil
	}
	if ok {
		return bound
	}
	e := sessionstore.Entry{Issues: issues}
	if err := b.sessions.Set(threadID, e); err != nil {
		log.Printf("warn: bind issues create thread=%s: %v", threadID, err)
		return nil
	}
	return issues
}

// resolveLinearIssues fills title/state/url/id when the project has an API key.
func (b *Bot) resolveLinearIssues(project string, issues []sessionstore.TrackedIssue) {
	if b == nil || b.cfg == nil || !b.cfg.ProjectLinearCanResolve(project) {
		return
	}
	key := b.cfg.ProjectLinearAPIKey(project)
	client := linear.New(key)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	for i := range issues {
		if !issues[i].IsLinear() || issues[i].Identifier == "" {
			continue
		}
		got, err := client.GetByIdentifier(ctx, issues[i].Identifier)
		if err != nil {
			log.Printf("warn: linear resolve %s project=%s: %v", issues[i].Identifier, project, err)
			continue
		}
		issues[i].LinearID = got.ID
		issues[i].Identifier = sessionstore.NormalizeLinearIdentifier(got.Identifier)
		if got.Title != "" {
			issues[i].Title = got.Title
		}
		if got.State != "" {
			issues[i].State = got.State
		}
		if got.URL != "" {
			issues[i].URL = got.URL
		}
		if got.TeamKey != "" {
			issues[i].TeamKey = got.TeamKey
		}
	}
}

// defaultIssueRepo returns owner, repo for bare issue numbers from session PRs.
func defaultIssueRepo(e sessionstore.Entry) (owner, repo string) {
	e.NormalizePRs()
	if p, ok := e.PrimaryPR(); ok && p.Owner != "" && p.Repo != "" {
		return p.Owner, p.Repo
	}
	for _, p := range e.PRs {
		if p.Owner != "" && p.Repo != "" {
			return p.Owner, p.Repo
		}
	}
	for _, iss := range e.Issues {
		if iss.Owner != "" && iss.Repo != "" {
			return iss.Owner, iss.Repo
		}
	}
	return "", ""
}

func (b *Bot) handleLink(s *discordgo.Session, m *discordgo.MessageCreate, parsed Parsed) {
	if !isThread(s, m.ChannelID) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, "Use `@Grok /link` inside a Grok thread.", ref(m)); err != nil {
			log.Printf("error: reply link-not-thread: %v", err)
		}
		return
	}
	threadID := m.ChannelID
	arg := parseLinkArg(parsed.Prompt)

	e, ok := b.sessions.Get(threadID)
	if !ok {
		parentID := parentChannelID(s, threadID)
		projName := ""
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
		e = sessionstore.Entry{Project: projName}
		if m.Author != nil {
			ensureSessionOwner(&e, m.Author.ID, m.Author.String())
		}
	}

	switch {
	case arg == "" || arg == "list" || arg == "show":
		msg := formatLinkStatus(e)
		if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
			log.Printf("error: reply link-status: %v", err)
		}
		return
	case arg == "help" || arg == "?":
		if _, err := s.ChannelMessageSendReply(threadID, linkHelpText(), ref(m)); err != nil {
			log.Printf("error: reply link-help: %v", err)
		}
		return
	case arg == "clear" || arg == "none" || arg == "reset":
		e.ClearIssues()
		if m.Author != nil {
			ensureSessionOwner(&e, m.Author.ID, m.Author.String())
		}
		if e.Project == "" {
			parentID := parentChannelID(s, threadID)
			if p, err := b.resolveProject(parentID); err == nil {
				e.Project = p.Name
			}
		}
		if err := b.sessions.Set(threadID, e); err != nil {
			if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not clear issues: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply link-clear: %v", sendErr)
			}
			return
		}
		if _, err := s.ChannelMessageSendReply(threadID, "Cleared linked issues for this thread.", ref(m)); err != nil {
			log.Printf("error: reply link-cleared: %v", err)
		}
		b.maybeRefreshBriefIssues(s, threadID)
		return
	}

	// /link unlink #42  or  /unlink #42 (handled as KindLink with unlink prefix)
	unlinkArg, isUnlink := parseUnlinkArg(arg)
	if isUnlink {
		if unlinkArg == "" {
			if _, err := s.ChannelMessageSendReply(threadID, "Usage: `@Grok /unlink #42` or `@Grok /link unlink #42`", ref(m)); err != nil {
				log.Printf("error: reply unlink-usage: %v", err)
			}
			return
		}
		if !e.RemoveIssue(unlinkArg) {
			if _, err := s.ChannelMessageSendReply(threadID, fmt.Sprintf("No linked issue matching `%s`.", unlinkArg), ref(m)); err != nil {
				log.Printf("error: reply unlink-miss: %v", err)
			}
			return
		}
		if err := b.sessions.Set(threadID, e); err != nil {
			if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save: "+err.Error(), ref(m)); sendErr != nil {
				log.Printf("error: reply unlink-save: %v", sendErr)
			}
			return
		}
		if _, err := s.ChannelMessageSendReply(threadID, fmt.Sprintf("Unlinked `%s`.", unlinkArg), ref(m)); err != nil {
			log.Printf("error: reply unlink-ok: %v", err)
		}
		b.maybeRefreshBriefIssues(s, threadID)
		return
	}

	// Resolve project for Linear opt-in.
	parentID := parentChannelID(s, threadID)
	projName := e.Project
	if projName == "" {
		if p, err := b.resolveProject(parentID); err == nil {
			projName = p.Name
		}
	}

	// Optional leading keyword: fix|fixes|closes|refs …
	keyword, rest := splitLinkKeyword(arg)
	var refs []sessionstore.TrackedIssue

	// Prefer Linear when enabled and the arg looks like Linear.
	if b.cfg != nil && b.cfg.ProjectLinearEnabled(projName) {
		if lin := sessionstore.ParseLinearIssueRefs(rest); len(lin) > 0 {
			refs = lin
			b.resolveLinearIssues(projName, refs)
		}
	} else if looksLikeLinearRef(rest) {
		if _, err := s.ChannelMessageSendReply(threadID,
			fmt.Sprintf("Linear is not enabled for project **%s**. Enable it in config (`projects.*.linear.enabled`) with a per-project API key.",
				displayProjectName(projName)), ref(m)); err != nil {
			log.Printf("error: reply link-linear-off: %v", err)
		}
		return
	}

	if len(refs) == 0 {
		refs = sessionstore.ParseIssueRefs(rest)
		if len(refs) == 0 {
			// Allow bare number without #.
			refs = sessionstore.ParseIssueRefs("#" + strings.TrimSpace(rest))
		}
		owner, repo := defaultIssueRepo(e)
		sessionstore.FillIssueOwnerRepo(refs, owner, repo)
	}
	if len(refs) == 0 {
		if _, err := s.ChannelMessageSendReply(threadID,
			fmt.Sprintf("Could not parse issue from `%s`. %s", arg, linkHelpText()), ref(m)); err != nil {
			log.Printf("error: reply link-parse: %v", err)
		}
		return
	}
	if keyword != "" {
		for i := range refs {
			refs[i].Keyword = keyword
		}
	}

	for _, iss := range refs {
		// Explicit /link always applies the chosen keyword (including Refs after Fixes).
		e.UpsertIssueForceKeyword(iss)
	}
	if m.Author != nil {
		ensureSessionOwner(&e, m.Author.ID, m.Author.String())
	}
	if e.Project == "" {
		e.Project = projName
	}
	if err := b.sessions.Set(threadID, e); err != nil {
		if _, sendErr := s.ChannelMessageSendReply(threadID, "Could not save issues: "+err.Error(), ref(m)); sendErr != nil {
			log.Printf("error: reply link-save: %v", sendErr)
		}
		return
	}

	var parts []string
	for _, iss := range refs {
		// Re-read after upsert for merged keyword/url.
		if full, ok := e.FindIssue(iss.DisplayRef()); ok {
			iss = full
		}
		part := fmt.Sprintf("%s (%s)", iss.DisplayRef(), iss.EffectiveKeyword())
		if iss.IsLinear() && iss.State != "" {
			part += " · " + iss.State
		}
		if iss.IsLinear() && iss.LinearID == "" && b.cfg != nil && b.cfg.ProjectLinearEnabled(projName) && !b.cfg.ProjectLinearCanResolve(projName) {
			part += " · unresolved (no API key)"
		}
		parts = append(parts, part)
	}
	msg := "Linked " + strings.Join(parts, ", ") + "."
	if _, err := s.ChannelMessageSendReply(threadID, msg, ref(m)); err != nil {
		log.Printf("error: reply link-ok: %v", err)
	}
	b.maybeRefreshBriefIssues(s, threadID)
}

func looksLikeLinearRef(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return len(sessionstore.ParseLinearIssueRefs(s)) > 0
}

func displayProjectName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unknown)"
	}
	return name
}

func parseLinkArg(prompt string) string {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, prefix := range []string{"/link", "link", "/unlink", "unlink"} {
		if lower == prefix {
			return ""
		}
		if strings.HasPrefix(lower, prefix+" ") {
			rest := strings.TrimSpace(text[len(prefix):])
			// Preserve unlink as a subcommand token for /link unlink …
			if prefix == "/unlink" || prefix == "unlink" {
				return "unlink " + rest
			}
			return rest
		}
	}
	return strings.TrimSpace(text)
}

// parseUnlinkArg returns (query, true) for "unlink …" args.
func parseUnlinkArg(arg string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(arg))
	if lower == "unlink" {
		return "", true
	}
	if strings.HasPrefix(lower, "unlink ") {
		return strings.TrimSpace(arg[len("unlink "):]), true
	}
	return "", false
}

// splitLinkKeyword peels optional Fixes/Refs keyword from the start of /link args.
func splitLinkKeyword(arg string) (keyword, rest string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", ""
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return "", arg
	}
	kw := sessionstore.NormalizeIssueKeyword(fields[0])
	// Only treat as keyword when the token is a known alias (not a random word).
	switch strings.ToLower(fields[0]) {
	case "fix", "fixes", "close", "closes", "closed", "resolve", "resolves", "resolved",
		"ref", "refs", "reference", "references":
		if len(fields) == 1 {
			return kw, ""
		}
		return kw, strings.TrimSpace(arg[len(fields[0]):])
	default:
		return "", arg
	}
}

func formatLinkStatus(e sessionstore.Entry) string {
	if !e.HasIssues() {
		return "**issue:** (none linked)\n" + linkHelpText()
	}
	lines := sessionstore.FormatIssueStatusLines(e.Issues)
	lines = append(lines, linkHelpText())
	return strings.Join(lines, "\n")
}

func linkHelpText() string {
	return "Link: `@Grok /link #42` · `@Grok /link ENG-123` · `@Grok /link fix #42` · `@Grok /unlink #42` · `@Grok /link clear`"
}

func (b *Bot) maybeRefreshBriefIssues(s *discordgo.Session, threadID string) {
	if b == nil || s == nil || threadID == "" {
		return
	}
	e, ok := b.sessions.Get(threadID)
	if !ok || e.BriefMsgID == "" {
		return
	}
	if _, err := b.refreshBriefCard(s, threadID, e.Cwd); err != nil {
		log.Printf("brief: issue link refresh thread=%s: %v", threadID, err)
	}
}

// prefixThreadTitleWithIssues adds "#N " when missing.
func prefixThreadTitleWithIssues(title string, issues []sessionstore.TrackedIssue) string {
	pref := sessionstore.IssueTitlePrefix(issues)
	if pref == "" {
		return title
	}
	title = strings.TrimSpace(title)
	// Already starts with same numbers?
	trimPref := strings.TrimSpace(pref)
	if strings.HasPrefix(title, trimPref) || strings.HasPrefix(strings.ToLower(title), strings.ToLower(trimPref)) {
		return title
	}
	// Avoid double #N if title already begins with #digits.
	combined := pref + title
	if len(combined) > 100 {
		// Discord thread name limit ~100; keep prefix and trim rest.
		rest := title
		maxRest := 100 - len(pref)
		if maxRest < 10 {
			return truncateRunes(pref, 100)
		}
		if len(rest) > maxRest {
			cut := strings.LastIndex(rest[:maxRest], " ")
			if cut < maxRest/3 {
				cut = maxRest
			}
			rest = strings.TrimSpace(rest[:cut])
		}
		return pref + rest
	}
	return combined
}
