package bot

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

var fencedJSONRE = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// ParseDossierFromReply extracts a dossier JSON object from model output.
// Looks for fenced ```json blocks with known keys; returns nil if none.
func ParseDossierFromReply(text string) *sessionstore.Dossier {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// Prefer fenced blocks
	matches := fencedJSONRE.FindAllStringSubmatch(text, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if d := tryParseDossierJSON(matches[i][1]); d != nil {
			return d
		}
	}
	// Bare JSON object containing "summary"
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			if d := tryParseDossierJSON(text[i : j+1]); d != nil {
				return d
			}
		}
	}
	return nil
}

func tryParseDossierJSON(raw string) *sessionstore.Dossier {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	// Must look like a dossier
	if _, ok := m["summary"]; !ok {
		if _, ok2 := m["hypotheses"]; !ok2 {
			if _, ok3 := m["nextActions"]; !ok3 {
				return nil
			}
		}
	}
	var d sessionstore.Dossier
	b, _ := json.Marshal(m)
	if err := json.Unmarshal(b, &d); err != nil {
		return nil
	}
	if d.Summary == "" && len(d.Hypotheses) == 0 && len(d.NextActions) == 0 {
		return nil
	}
	d.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return &d
}

// MergeDossier overlays non-empty fields from src onto dst.
func MergeDossier(dst *sessionstore.Dossier, src *sessionstore.Dossier) *sessionstore.Dossier {
	if src == nil {
		return dst
	}
	if dst == nil {
		cp := *src
		return &cp
	}
	if src.Summary != "" {
		dst.Summary = src.Summary
	}
	if src.Environment != "" {
		dst.Environment = src.Environment
	}
	if len(src.ReproSteps) > 0 {
		dst.ReproSteps = append([]string(nil), src.ReproSteps...)
	}
	if len(src.Evidence) > 0 {
		dst.Evidence = append([]string(nil), src.Evidence...)
	}
	if len(src.Hypotheses) > 0 {
		dst.Hypotheses = append([]string(nil), src.Hypotheses...)
	}
	if len(src.KnownBugHits) > 0 {
		dst.KnownBugHits = append([]string(nil), src.KnownBugHits...)
	}
	if len(src.NextActions) > 0 {
		dst.NextActions = append([]string(nil), src.NextActions...)
	}
	if src.UpdatedAt != "" {
		dst.UpdatedAt = src.UpdatedAt
	} else {
		dst.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return dst
}
