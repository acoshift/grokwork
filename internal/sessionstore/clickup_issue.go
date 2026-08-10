package sessionstore

import (
	"fmt"
	"regexp"
	"strings"
)

// ProviderClickUp is TrackedIssue.Provider for ClickUp tasks.
const ProviderClickUp = "clickup"

var (
	// https://app.clickup.com/t/{nativeId}  (not pure-digit workspace segment of custom URLs)
	clickupNativeURLRE = regexp.MustCompile(`(?i)https?://(?:app\.)?clickup\.com/t/([A-Za-z0-9]+)\b`)
	// https://app.clickup.com/t/{workspaceId}/{CUSTOM-ID}
	clickupCustomURLRE = regexp.MustCompile(`(?i)https?://(?:app\.)?clickup\.com/t/(\d+)/([A-Za-z][A-Za-z0-9]*-\d+)\b`)
	// PREFIX-N when prefix is known at call site
	clickupCustomIDRE = regexp.MustCompile(`(?i)\b([A-Za-z][A-Za-z0-9]*)-(\d+)\b`)
	// Bare native id query (Find/Remove/link); min 3 chars so short real ids still work.
	clickupNativeIDQueryRE = regexp.MustCompile(`(?i)^[a-z0-9]{3,}$`)
)

// IsClickUp reports whether this tracked issue is a ClickUp task.
func (iss TrackedIssue) IsClickUp() bool {
	return strings.EqualFold(strings.TrimSpace(iss.Provider), ProviderClickUp)
}

// NormalizeClickUpCustomID uppercases the prefix: dev-42 → DEV-42.
func NormalizeClickUpCustomID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return strings.ToUpper(id)
	}
	return strings.ToUpper(parts[0]) + "-" + parts[1]
}

// ParseClickUpIssueRefs extracts ClickUp task refs from free text.
// prefix is the configured customIdPrefix (empty → bare PREFIX-N not parsed).
func ParseClickUpIssueRefs(text, prefix string) []TrackedIssue {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "<", " ")
	text = strings.ReplaceAll(text, ">", " ")

	type hit struct {
		iss   TrackedIssue
		start int
	}
	var hits []hit
	seen := map[string]struct{}{}

	add := func(iss TrackedIssue, start int) {
		iss.Provider = ProviderClickUp
		if cid := NormalizeClickUpCustomID(iss.CustomID); cid != "" {
			iss.CustomID = cid
		}
		iss.ClickUpID = strings.TrimSpace(iss.ClickUpID)
		iss.URL = strings.TrimSpace(iss.URL)
		key := iss.IssueKey()
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		// Prefer richer keys: skip bare custom if native already seen and vice versa only by IssueKey.
		seen[key] = struct{}{}
		if iss.Keyword == "" {
			iss.Keyword = keywordBefore(text, start)
		} else {
			iss.Keyword = NormalizeIssueKeyword(iss.Keyword)
		}
		hits = append(hits, hit{iss: iss, start: start})
	}

	// Custom-id URLs first (two segments) so they win over native single-segment regex.
	for _, m := range clickupCustomURLRE.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 6 {
			continue
		}
		ws := text[m[2]:m[3]]
		custom := NormalizeClickUpCustomID(text[m[4]:m[5]])
		full := text[m[0]:m[1]]
		// Trim trailing delimiter if regex consumed it
		url := strings.TrimRight(full, "/?#")
		add(TrackedIssue{
			Provider:    ProviderClickUp,
			CustomID:    custom,
			WorkspaceID: ws,
			URL:         url,
		}, m[0])
	}

	for _, m := range clickupNativeURLRE.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 4 {
			continue
		}
		// Skip if this match is the workspace id segment of a custom URL
		// (custom URL already handled; native regex may also match first segment).
		id := text[m[2]:m[3]]
		// Pure digits alone in /t/{n}/… are workspace ids handled above.
		if isAllDigits(id) {
			continue
		}
		full := text[m[0]:m[1]]
		url := strings.TrimRight(full, "/?#")
		add(TrackedIssue{
			Provider:  ProviderClickUp,
			ClickUpID: id,
			URL:       url,
		}, m[0])
	}

	// Bare PREFIX-N must not match inside URLs (Linear or ClickUp links).
	bareText := maskURLsForBareParse(text)
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if prefix != "" {
		for _, m := range clickupCustomIDRE.FindAllStringSubmatchIndex(bareText, -1) {
			if len(m) < 6 {
				continue
			}
			p := strings.ToUpper(text[m[2]:m[3]])
			if p != prefix {
				continue
			}
			num := text[m[4]:m[5]]
			custom := p + "-" + num
			add(TrackedIssue{
				Provider: ProviderClickUp,
				CustomID: custom,
			}, m[0])
		}
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

// maskURLsForBareParse replaces http(s) URL spans with spaces so bare PREFIX-N
// regexes cannot match inside link paths (e.g. linear.app/.../DEV-7 or clickup .../DEV-9).
// Length-preserving so match indices remain valid against the original text for keywordBefore.
func maskURLsForBareParse(text string) string {
	b := []byte(text)
	for i := 0; i < len(b); i++ {
		if !hasHTTPPrefix(b, i) {
			continue
		}
		j := i
		for j < len(b) && b[j] != ' ' && b[j] != '\t' && b[j] != '\n' && b[j] != '\r' {
			b[j] = ' '
			j++
		}
		i = j
	}
	return string(b)
}

func hasHTTPPrefix(b []byte, i int) bool {
	// http:// or https://
	if i+7 <= len(b) && (b[i] == 'h' || b[i] == 'H') {
		s := strings.ToLower(string(b[i:min(i+8, len(b))]))
		return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func clickupIssueKey(iss TrackedIssue) string {
	if cid := NormalizeClickUpCustomID(iss.CustomID); cid != "" {
		return "clickup:custom:" + strings.ToLower(cid)
	}
	if id := strings.TrimSpace(iss.ClickUpID); id != "" {
		return "clickup:id:" + strings.ToLower(id)
	}
	if u := strings.TrimSpace(iss.URL); u != "" {
		return "clickup-url:" + strings.ToLower(strings.TrimRight(u, "/"))
	}
	return ""
}

func formatClickUpDisplay(iss TrackedIssue) string {
	if cid := NormalizeClickUpCustomID(iss.CustomID); cid != "" {
		return cid
	}
	if id := strings.TrimSpace(iss.ClickUpID); id != "" {
		return id
	}
	return strings.TrimSpace(iss.URL)
}

func clickupPRBodyLine(iss TrackedIssue) string {
	kw := iss.EffectiveKeyword()
	if d := formatClickUpDisplay(iss); d != "" && !strings.HasPrefix(strings.ToLower(d), "http") {
		return fmt.Sprintf("%s %s", kw, d)
	}
	if u := strings.TrimSpace(iss.URL); u != "" {
		return kw + " " + u
	}
	return ""
}

// parseClickUpQuery tries to interpret a /link or /unlink query as ClickUp.
// prefix is optional; empty still allows URL and any PREFIX-N shape as candidate.
func parseClickUpQuery(query, prefix string) (TrackedIssue, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return TrackedIssue{}, false
	}
	query = strings.TrimPrefix(query, "<")
	query = strings.TrimSuffix(query, ">")
	// Prefer URL parse with empty prefix (URLs don't need prefix).
	parsed := ParseClickUpIssueRefs(query, prefix)
	if len(parsed) == 0 && prefix == "" {
		// Try with a synthetic prefix from the query itself for PREFIX-N.
		if m := clickupCustomIDRE.FindStringSubmatch(query); len(m) == 3 {
			parsed = ParseClickUpIssueRefs(query, m[1])
		}
	}
	if len(parsed) > 0 {
		return parsed[0], true
	}
	// Bare native id (no free-text auto-bind elsewhere): only when it doesn't look like PREFIX-N or #N.
	q := strings.TrimSpace(query)
	if strings.HasPrefix(q, "#") {
		return TrackedIssue{}, false
	}
	if clickupCustomIDRE.MatchString(q) {
		// Already handled if prefix matched; for query matching allow any PREFIX-N as ClickUp candidate.
		parts := strings.SplitN(q, "-", 2)
		if len(parts) == 2 {
			return TrackedIssue{
				Provider: ProviderClickUp,
				CustomID: NormalizeClickUpCustomID(q),
			}, true
		}
	}
	// Alphanumeric native id (not pure digits — those stay GitHub #N).
	if clickupNativeIDQueryRE.MatchString(q) && !isAllDigits(q) {
		return TrackedIssue{Provider: ProviderClickUp, ClickUpID: q}, true
	}
	return TrackedIssue{}, false
}

// sameClickUpIssue reports whether two ClickUp issues refer to the same task.
func sameClickUpIssue(a, b TrackedIssue) bool {
	if a.ClickUpID != "" && b.ClickUpID != "" && strings.EqualFold(a.ClickUpID, b.ClickUpID) {
		return true
	}
	ca, cb := NormalizeClickUpCustomID(a.CustomID), NormalizeClickUpCustomID(b.CustomID)
	if ca != "" && cb != "" && strings.EqualFold(ca, cb) {
		return true
	}
	ka, kb := clickupIssueKey(a), clickupIssueKey(b)
	return ka != "" && ka == kb
}
