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
func ParseFindings(text string) (summary string, findings []Finding, err error) {
	raw := extractJSONObject(text)
	if raw == "" {
		return "", nil, fmt.Errorf("no JSON object in model response")
	}
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

// extractJSONObject returns the first top-level JSON object substring.
func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	// Strip markdown fence if present.
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(strings.ToLower(rest), "json") {
			rest = strings.TrimSpace(rest[4:])
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		text = strings.TrimSpace(rest)
	}
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
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
				return text[start : i+1]
			}
		}
	}
	return ""
}
