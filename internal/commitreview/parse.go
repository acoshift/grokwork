package commitreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MaxFindings is the hard cap of issues filed per review.
const MaxFindings = 15

var severityRank = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
	"info":     4,
}

// ParseFindings extracts summary + findings from model text (JSON or fenced JSON).
//
// Models sometimes emit an intermediate object mid-review then a final one, or put
// markdown fences inside finding bodies. We collect every top-level JSON object
// (string-aware brace scan) and use the last one that unmarshals as findings.
func ParseFindings(text string) (summary string, findings []Finding, err error) {
	objs := extractJSONObjects(text)
	if len(objs) == 0 {
		return "", nil, fmt.Errorf("no JSON object in model response")
	}
	var lastErr error
	for i := len(objs) - 1; i >= 0; i-- {
		sum, fs, perr := parseFindingsObject(objs[i])
		if perr != nil {
			lastErr = perr
			continue
		}
		return sum, fs, nil
	}
	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, fmt.Errorf("no JSON object in model response")
}

func parseFindingsObject(raw string) (summary string, findings []Finding, err error) {
	var v struct {
		Summary  string `json:"summary"`
		Findings []struct {
			Title    string   `json:"title"`
			Body     string   `json:"body"`
			Severity string   `json:"severity"`
			Paths    []string `json:"paths"`
			Labels   []string `json:"labels"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", nil, fmt.Errorf("parse findings JSON: %w", err)
	}
	// Reject unrelated top-level objects (e.g. tool payloads) that lack both fields.
	if strings.TrimSpace(v.Summary) == "" && len(v.Findings) == 0 {
		// Empty findings with empty summary is still a valid "looks fine" review only
		// if the key "findings" was present. encoding/json cannot tell omitted vs [].
		// Require at least one known key via a second generic map check.
		var probe map[string]json.RawMessage
		if jerr := json.Unmarshal([]byte(raw), &probe); jerr != nil {
			return "", nil, fmt.Errorf("parse findings JSON: %w", jerr)
		}
		if _, hasSum := probe["summary"]; !hasSum {
			if _, hasFind := probe["findings"]; !hasFind {
				return "", nil, fmt.Errorf("parse findings JSON: not a findings object")
			}
		}
	}
	summary = strings.TrimSpace(v.Summary)
	out := make([]Finding, 0, len(v.Findings))
	for _, f := range v.Findings {
		title := strings.TrimSpace(f.Title)
		if title == "" {
			continue
		}
		if len(title) > 80 {
			title = title[:80]
		}
		sev := normalizeSeverity(f.Severity)
		body := strings.TrimSpace(f.Body)
		if body == "" {
			body = title
		}
		paths := cleanStrings(f.Paths)
		labels := cleanStrings(f.Labels)
		fp := fingerprint(sev, title, paths)
		out = append(out, Finding{
			Title:       title,
			Body:        body,
			Severity:    sev,
			Paths:       paths,
			Labels:      labels,
			Fingerprint: fp,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank[out[i].Severity] < severityRank[out[j].Severity]
	})
	if len(out) > MaxFindings {
		out = out[:MaxFindings]
	}
	return summary, out, nil
}

func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "medium"
}

func cleanStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func fingerprint(severity, title string, paths []string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(severity))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.ToLower(title)))
	_, _ = h.Write([]byte{0})
	ps := append([]string(nil), paths...)
	sort.Strings(ps)
	for _, p := range ps {
		_, _ = h.Write([]byte(strings.ToLower(p)))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// extractJSONObjects returns all top-level JSON object substrings (string-aware).
// Markdown fences inside string values are ignored. If no objects are found,
// falls back to scanning after a leading ``` / ```json fence wrapper.
func extractJSONObjects(text string) []string {
	text = strings.TrimSpace(text)
	if objs := scanJSONObjects(text); len(objs) > 0 {
		return objs
	}
	if fenced, ok := unwrapMarkdownFence(text); ok {
		return scanJSONObjects(fenced)
	}
	return nil
}

// extractJSONObject returns the first top-level JSON object (tests / callers).
func extractJSONObject(text string) string {
	objs := extractJSONObjects(text)
	if len(objs) == 0 {
		return ""
	}
	return objs[0]
}

// unwrapMarkdownFence strips a leading ``` or ```json wrapper only when the
// fence appears before any top-level '{'. Does not touch fences inside bodies.
func unwrapMarkdownFence(text string) (string, bool) {
	text = strings.TrimSpace(text)
	brace := strings.Index(text, "{")
	fence := strings.Index(text, "```")
	if fence < 0 || (brace >= 0 && brace < fence) {
		return "", false
	}
	rest := strings.TrimSpace(text[fence+3:])
	if strings.HasPrefix(strings.ToLower(rest), "json") {
		rest = strings.TrimSpace(rest[4:])
	}
	if j := strings.Index(rest, "```"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest), true
}

func scanJSONObjects(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end := scanJSONObjectEnd(text, i)
		if end < 0 {
			continue
		}
		out = append(out, text[i:end])
		i = end - 1
	}
	return out
}

// scanJSONObjectEnd returns the index just past the matching '}', or -1.
func scanJSONObjectEnd(text string, start int) int {
	if start < 0 || start >= len(text) || text[start] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
