package bot

import (
	"regexp"
	"strings"
)

var (
	reAbsUnixPath = regexp.MustCompile(`(?i)(^|[\s\x60"'=(])/(?:Users|home|var|tmp|private|opt|usr)/[^\s\x60"')]+`)
	reAbsWinPath  = regexp.MustCompile(`(?i)[A-Za-z]:\\[^\s\x60"')]+`)
	reWorktree    = regexp.MustCompile(`(?i)data/worktrees|grok/discord/|grok/web/`)
	reTokenish    = regexp.MustCompile(`(?i)\b(GH_TOKEN|GITHUB_TOKEN|sk-[A-Za-z0-9_-]{10,}|xox[baprs]-[A-Za-z0-9-]{10,}|Bearer\s+[A-Za-z0-9._\-]{12,})\b`)
)

// SanitizeCustomerUpdate strips internal paths/secrets from customer-facing text.
// Returns cleaned text and human-readable hit kinds (not secret values).
func SanitizeCustomerUpdate(raw string) (clean string, hits []string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	// Prefer CUSTOMER_UPDATE: block if present.
	if i := strings.Index(strings.ToUpper(s), "CUSTOMER_UPDATE:"); i >= 0 {
		s = strings.TrimSpace(s[i+len("CUSTOMER_UPDATE:"):])
	}

	hitSet := map[string]bool{}
	replace := func(re *regexp.Regexp, kind string) {
		if re.MatchString(s) {
			hitSet[kind] = true
			s = re.ReplaceAllStringFunc(s, func(m string) string {
				// Keep leading delimiter if captured
				if len(m) > 0 && (m[0] == ' ' || m[0] == '\t' || m[0] == '\n' || m[0] == '`' || m[0] == '"' || m[0] == '\'') {
					return string(m[0]) + "[redacted]"
				}
				if strings.HasPrefix(m, "(") || strings.HasPrefix(m, "=") {
					return string(m[0]) + "[redacted]"
				}
				return "[redacted]"
			})
		}
	}
	replace(reAbsUnixPath, "absolute_path")
	replace(reAbsWinPath, "windows_path")
	replace(reWorktree, "worktree_path")
	replace(reTokenish, "secret")

	for k := range hitSet {
		hits = append(hits, k)
	}
	// stable-ish order
	if len(hits) > 1 {
		// simple bubble for few kinds
		for i := 0; i < len(hits); i++ {
			for j := i + 1; j < len(hits); j++ {
				if hits[j] < hits[i] {
					hits[i], hits[j] = hits[j], hits[i]
				}
			}
		}
	}
	return strings.TrimSpace(s), hits
}
