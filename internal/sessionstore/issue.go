package sessionstore

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// GitHub issue keywords for PR body convention (Fixes closes; Refs links only).
const (
	IssueKeywordFixes = "Fixes"
	IssueKeywordRefs  = "Refs"
)

const maxTrackedIssues = 5

// TrackedIssue is a GitHub or Linear ticket bound to a Discord thread.
type TrackedIssue struct {
	// Provider is "github" (default/empty) or "linear".
	Provider string `json:"provider,omitempty"`

	// GitHub fields
	Number  int    `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
	// Keyword is "Fixes" (close on merge) or "Refs" (link only). Empty → Refs.
	Keyword string `json:"keyword,omitempty"`

	// Linear fields (Provider == "linear")
	Identifier string `json:"identifier,omitempty"` // ENG-123
	LinearID   string `json:"linearId,omitempty"`   // UUID from API
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	TeamKey    string `json:"teamKey,omitempty"`
}

var (
	githubIssueURLRE = regexp.MustCompile(`(?i)https?://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/(\d+)`)
	// owner/repo#42 — not a URL; used in chat and gh style.
	ownerRepoIssueRE = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#(\d+)\b`)
	// Bare #42 with a non-word char (or start) before the hash so we skip colors/ids mid-token.
	bareIssueRE = regexp.MustCompile(`(?:^|[^\w/])#(\d+)\b`)
	// Close-intent words immediately before a ref (within a short window).
	closeIntentRE = regexp.MustCompile(`(?i)\b(fix(?:es|ed)?|close[sd]?|resolve[sd]?)\b`)
	refsIntentRE  = regexp.MustCompile(`(?i)\b(refs?|regarding|about|re)\b`)
)

// IssueKey returns a stable identity for matching tracked issues.
func (iss TrackedIssue) IssueKey() string {
	if iss.IsLinear() {
		if k := linearIssueKey(iss.Identifier); k != "" {
			return k
		}
		if u := strings.TrimSpace(iss.URL); u != "" {
			return "linear-url:" + strings.ToLower(strings.TrimRight(u, "/"))
		}
		return ""
	}
	if iss.Owner != "" && iss.Repo != "" && iss.Number > 0 {
		return strings.ToLower(fmt.Sprintf("%s/%s#%d", iss.Owner, iss.Repo, iss.Number))
	}
	if u := strings.TrimSpace(iss.URL); u != "" {
		return strings.ToLower(strings.TrimRight(u, "/"))
	}
	if iss.Number > 0 {
		return fmt.Sprintf("#%d", iss.Number)
	}
	return ""
}

// RepoSlug returns owner/repo when known.
func (iss TrackedIssue) RepoSlug() string {
	if iss.Owner != "" && iss.Repo != "" {
		return iss.Owner + "/" + iss.Repo
	}
	return ""
}

// DisplayRef is a short human form: ENG-123, owner/repo#N, or #N.
func (iss TrackedIssue) DisplayRef() string {
	if iss.IsLinear() {
		return formatLinearDisplay(iss)
	}
	if slug := iss.RepoSlug(); slug != "" && iss.Number > 0 {
		return fmt.Sprintf("%s#%d", slug, iss.Number)
	}
	if iss.Number > 0 {
		return fmt.Sprintf("#%d", iss.Number)
	}
	return strings.TrimSpace(iss.URL)
}

// EffectiveKeyword returns Fixes or Refs (default Refs).
func (iss TrackedIssue) EffectiveKeyword() string {
	switch strings.ToLower(strings.TrimSpace(iss.Keyword)) {
	case "fix", "fixes", "close", "closes", "closed", "resolve", "resolves", "resolved":
		return IssueKeywordFixes
	default:
		return IssueKeywordRefs
	}
}

// PRBodyLine is the PR body convention line, e.g. "Fixes #42" or "Fixes ENG-123".
func (iss TrackedIssue) PRBodyLine() string {
	if iss.IsLinear() {
		return linearPRBodyLine(iss)
	}
	kw := iss.EffectiveKeyword()
	if iss.Number <= 0 {
		if u := strings.TrimSpace(iss.URL); u != "" {
			return kw + " " + u
		}
		return ""
	}
	// owner/repo#N works same-repo and cross-repo; bare #N when repo unknown.
	if iss.Owner != "" && iss.Repo != "" {
		return fmt.Sprintf("%s %s/%s#%d", kw, iss.Owner, iss.Repo, iss.Number)
	}
	return fmt.Sprintf("%s #%d", kw, iss.Number)
}

// FillFromURL parses owner/repo/number from URL when missing.
func (iss *TrackedIssue) FillFromURL() {
	if iss == nil {
		return
	}
	m := githubIssueURLRE.FindStringSubmatch(iss.URL)
	if len(m) < 4 {
		// Build URL when we have parts.
		if iss.URL == "" && iss.Owner != "" && iss.Repo != "" && iss.Number > 0 {
			iss.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", iss.Owner, iss.Repo, iss.Number)
		}
		return
	}
	if iss.Owner == "" {
		iss.Owner = m[1]
	}
	if iss.Repo == "" {
		iss.Repo = m[2]
	}
	if iss.Number <= 0 {
		if n, err := strconv.Atoi(m[3]); err == nil {
			iss.Number = n
		}
	}
	if iss.URL == "" && iss.Owner != "" && iss.Repo != "" && iss.Number > 0 {
		iss.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", iss.Owner, iss.Repo, iss.Number)
	} else if iss.URL != "" {
		// Canonicalize.
		iss.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", m[1], m[2], mustAtoi(m[3]))
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// NormalizeKeyword canonicalizes to Fixes or Refs.
func NormalizeIssueKeyword(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fix", "fixes", "close", "closes", "closed", "resolve", "resolves", "resolved":
		return IssueKeywordFixes
	case "ref", "refs", "reference", "references", "see", "regarding":
		return IssueKeywordRefs
	default:
		return IssueKeywordRefs
	}
}

// SameIssue reports whether two tracked issues refer to the same ticket.
func SameIssue(a, b TrackedIssue) bool { return sameIssue(a, b) }

// sameIssue reports whether two tracked issues refer to the same ticket.
func sameIssue(a, b TrackedIssue) bool {
	// Never match across providers.
	if a.IsLinear() != b.IsLinear() {
		return false
	}
	if a.IsLinear() {
		if a.IssueKey() != "" && a.IssueKey() == b.IssueKey() {
			return true
		}
		if a.LinearID != "" && a.LinearID == b.LinearID {
			return true
		}
		return false
	}
	if a.Number > 0 && a.Number == b.Number {
		if a.Owner != "" && b.Owner != "" && a.Repo != "" && b.Repo != "" {
			return strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Repo, b.Repo)
		}
		// Bare number matches when either side lacks owner (same-repo assumption).
		if a.Owner == "" || b.Owner == "" {
			return true
		}
	}
	if a.IssueKey() != "" && a.IssueKey() == b.IssueKey() {
		return true
	}
	return false
}

// UpsertIssue inserts or updates a tracked issue (match by owner/repo#n or bare #n).
// Keyword upgrades Refs → Fixes on re-parse; use UpsertIssueForceKeyword to set Refs after Fixes.
func (e *Entry) UpsertIssue(iss TrackedIssue) {
	e.upsertIssue(iss, false)
}

// UpsertIssueForceKeyword is like UpsertIssue but always applies the provided keyword
// (for explicit @Grok /link fix|refs …).
func (e *Entry) UpsertIssueForceKeyword(iss TrackedIssue) {
	e.upsertIssue(iss, true)
}

func (e *Entry) upsertIssue(iss TrackedIssue, forceKeyword bool) {
	if e == nil {
		return
	}
	if iss.IsLinear() {
		iss.Provider = ProviderLinear
		iss.Identifier = NormalizeLinearIdentifier(iss.Identifier)
		if iss.Identifier == "" && strings.TrimSpace(iss.URL) == "" {
			return
		}
		if team, _, ok := splitLinearIdentifier(iss.Identifier); ok && iss.TeamKey == "" {
			iss.TeamKey = team
		}
	} else {
		iss.FillFromURL()
		if iss.Number <= 0 && strings.TrimSpace(iss.URL) == "" {
			return
		}
	}
	if iss.Keyword == "" {
		iss.Keyword = IssueKeywordRefs
	} else {
		iss.Keyword = NormalizeIssueKeyword(iss.Keyword)
	}

	for i := range e.Issues {
		if sameIssue(e.Issues[i], iss) {
			prev := e.Issues[i]
			if iss.IsLinear() {
				if iss.Identifier == "" {
					iss.Identifier = prev.Identifier
				}
				if iss.LinearID == "" {
					iss.LinearID = prev.LinearID
				}
				if iss.URL == "" {
					iss.URL = prev.URL
				}
				if iss.Title == "" {
					iss.Title = prev.Title
				}
				if iss.State == "" {
					iss.State = prev.State
				}
				if iss.TeamKey == "" {
					iss.TeamKey = prev.TeamKey
				}
			} else {
				// Fill missing owner/repo/url from either side.
				if iss.Owner == "" {
					iss.Owner = prev.Owner
				}
				if iss.Repo == "" {
					iss.Repo = prev.Repo
				}
				if iss.URL == "" {
					iss.URL = prev.URL
				}
				if iss.Number <= 0 {
					iss.Number = prev.Number
				}
				iss.FillFromURL()
			}
			// Auto-parse: prefer Fixes over Refs. Manual /link may force either.
			if !forceKeyword && prev.EffectiveKeyword() == IssueKeywordFixes && iss.EffectiveKeyword() != IssueKeywordFixes {
				iss.Keyword = IssueKeywordFixes
			}
			e.Issues[i] = iss
			return
		}
	}
	if len(e.Issues) >= maxTrackedIssues {
		// Drop oldest to make room.
		e.Issues = e.Issues[1:]
	}
	e.Issues = append(e.Issues, iss)
}

// RemoveIssue drops a tracked issue by query (URL, #n, owner/repo#n, ENG-123).
func (e *Entry) RemoveIssue(query string) bool {
	if e == nil {
		return false
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	var target TrackedIssue
	if lin, ok := parseLinearQuery(query); ok {
		target = lin
	} else {
		parsed := ParseIssueRefs(query)
		if len(parsed) > 0 {
			target = parsed[0]
		} else if n, err := strconv.Atoi(strings.TrimPrefix(query, "#")); err == nil {
			target = TrackedIssue{Number: n}
		} else {
			return false
		}
	}
	out := e.Issues[:0]
	removed := false
	for _, iss := range e.Issues {
		if sameIssue(iss, target) {
			removed = true
			continue
		}
		out = append(out, iss)
	}
	e.Issues = out
	return removed
}

// ClearIssues removes all bound issues.
func (e *Entry) ClearIssues() {
	if e == nil {
		return
	}
	e.Issues = nil
}

// HasIssues reports whether any issue is bound.
func (e Entry) HasIssues() bool {
	return len(e.Issues) > 0
}

// FindIssue looks up a tracked issue by URL, #n, owner/repo#n, or ENG-123.
func (e Entry) FindIssue(query string) (TrackedIssue, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return TrackedIssue{}, false
	}
	var target TrackedIssue
	if lin, ok := parseLinearQuery(query); ok {
		target = lin
	} else {
		parsed := ParseIssueRefs(query)
		if len(parsed) > 0 {
			target = parsed[0]
		} else if n, err := strconv.Atoi(strings.TrimPrefix(query, "#")); err == nil {
			target = TrackedIssue{Number: n}
		} else {
			return TrackedIssue{}, false
		}
	}
	for _, iss := range e.Issues {
		if sameIssue(iss, target) {
			return iss, true
		}
	}
	return TrackedIssue{}, false
}

// FormatIssueStatusLines returns Discord lines for bound issues (empty if none).
func FormatIssueStatusLines(issues []TrackedIssue) []string {
	if len(issues) == 0 {
		return nil
	}
	lines := make([]string, 0, len(issues)+1)
	formatOne := func(iss TrackedIssue) string {
		line := fmt.Sprintf("%s (%s)", iss.DisplayRef(), iss.EffectiveKeyword())
		if iss.IsLinear() {
			if st := strings.TrimSpace(iss.State); st != "" {
				line += " · " + st
			}
		}
		if u := strings.TrimSpace(iss.URL); u != "" {
			line += " · " + u
		}
		return line
	}
	if len(issues) == 1 {
		lines = append(lines, "**issue:** "+formatOne(issues[0]))
		return lines
	}
	lines = append(lines, fmt.Sprintf("**issues:** (%d)", len(issues)))
	for _, iss := range issues {
		lines = append(lines, "• "+formatOne(iss))
	}
	return lines
}

// IssueTitlePrefix returns "#42 " and/or "ENG-123 " prefixes for Discord/PR titles.
func IssueTitlePrefix(issues []TrackedIssue) string {
	if len(issues) == 0 {
		return ""
	}
	seenGH := map[int]struct{}{}
	seenLin := map[string]struct{}{}
	var parts []string
	for _, iss := range issues {
		if iss.IsLinear() {
			id := NormalizeLinearIdentifier(iss.Identifier)
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if _, ok := seenLin[key]; ok {
				continue
			}
			seenLin[key] = struct{}{}
			parts = append(parts, id)
		} else {
			if iss.Number <= 0 {
				continue
			}
			if _, ok := seenGH[iss.Number]; ok {
				continue
			}
			seenGH[iss.Number] = struct{}{}
			parts = append(parts, fmt.Sprintf("#%d", iss.Number))
		}
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// ParseIssueRefs extracts GitHub issue references from free text.
// Detects full issue URLs, owner/repo#N, and bare #N. Keyword is Fixes when a
// close-intent word precedes the ref; otherwise Refs.
func ParseIssueRefs(text string) []TrackedIssue {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	type hit struct {
		iss   TrackedIssue
		start int
	}
	var hits []hit
	seen := map[string]struct{}{}

	add := func(iss TrackedIssue, start int) {
		iss.FillFromURL()
		if iss.Number <= 0 {
			return
		}
		key := iss.IssueKey()
		if key == "" {
			return
		}
		// Prefer richer key (with owner) over bare when both appear.
		if _, ok := seen[key]; ok {
			return
		}
		// Also skip bare #N when owner/repo#N already seen for same number.
		if iss.Owner == "" {
			for k := range seen {
				if strings.HasSuffix(k, fmt.Sprintf("#%d", iss.Number)) {
					return
				}
			}
		} else {
			// If bare #N was recorded first, replace it.
			bare := fmt.Sprintf("#%d", iss.Number)
			if _, ok := seen[bare]; ok {
				delete(seen, bare)
				filtered := hits[:0]
				for _, h := range hits {
					if h.iss.IssueKey() != bare {
						filtered = append(filtered, h)
					}
				}
				hits = filtered
			}
		}
		seen[key] = struct{}{}
		if iss.Keyword == "" {
			iss.Keyword = keywordBefore(text, start)
		} else {
			iss.Keyword = NormalizeIssueKeyword(iss.Keyword)
		}
		hits = append(hits, hit{iss: iss, start: start})
	}

	for _, m := range githubIssueURLRE.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 8 {
			continue
		}
		n, _ := strconv.Atoi(text[m[6]:m[7]])
		add(TrackedIssue{
			Owner:  text[m[2]:m[3]],
			Repo:   text[m[4]:m[5]],
			Number: n,
			URL:    fmt.Sprintf("https://github.com/%s/%s/issues/%d", text[m[2]:m[3]], text[m[4]:m[5]], n),
		}, m[0])
	}

	for _, m := range ownerRepoIssueRE.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 8 {
			continue
		}
		// Skip if this looks like it was already covered as part of a URL path.
		start := m[0]
		n, _ := strconv.Atoi(text[m[6]:m[7]])
		add(TrackedIssue{
			Owner:  text[m[2]:m[3]],
			Repo:   text[m[4]:m[5]],
			Number: n,
		}, start)
	}

	for _, m := range bareIssueRE.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 4 {
			continue
		}
		// bareIssueRE includes the leading non-word char in the full match; number is group 1.
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		// start of '#' for keyword window
		hashStart := m[2] - 1
		if hashStart < 0 {
			hashStart = m[0]
		}
		add(TrackedIssue{Number: n}, hashStart)
	}

	if len(hits) == 0 {
		return nil
	}
	out := make([]TrackedIssue, len(hits))
	for i, h := range hits {
		out[i] = h.iss
	}
	if len(out) > maxTrackedIssues {
		out = out[:maxTrackedIssues]
	}
	return out
}

// keywordBefore inspects a short window before pos for Fixes vs Refs intent.
func keywordBefore(text string, pos int) string {
	if pos < 0 {
		pos = 0
	}
	start := pos - 48
	if start < 0 {
		start = 0
	}
	window := text[start:pos]
	// Prefer the closest intent word.
	if closeIntentRE.MatchString(window) {
		// If both appear, last match wins by checking trailing half.
		ci := closeIntentRE.FindAllStringIndex(window, -1)
		ri := refsIntentRE.FindAllStringIndex(window, -1)
		lastClose := -1
		if len(ci) > 0 {
			lastClose = ci[len(ci)-1][0]
		}
		lastRefs := -1
		if len(ri) > 0 {
			lastRefs = ri[len(ri)-1][0]
		}
		if lastRefs > lastClose {
			return IssueKeywordRefs
		}
		return IssueKeywordFixes
	}
	if refsIntentRE.MatchString(window) {
		return IssueKeywordRefs
	}
	return IssueKeywordRefs
}

// FillIssueOwnerRepo sets Owner/Repo on bare issues from a default slug "owner/repo".
func FillIssueOwnerRepo(issues []TrackedIssue, owner, repo string) {
	if owner == "" || repo == "" {
		return
	}
	for i := range issues {
		if issues[i].Owner == "" {
			issues[i].Owner = owner
		}
		if issues[i].Repo == "" {
			issues[i].Repo = repo
		}
		issues[i].FillFromURL()
	}
}
