package sessionstore

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxDossierJSONBytes    = 32 * 1024
	maxCustomerUpdateRunes = 2000
	maxCustomerTitleRunes  = 200
	maxCustomerRefRunes    = 128
	maxResolutionNoteRunes = 500
)

// ClampCaseFields enforces size caps on case fields. Mutates e in place.
func ClampCaseFields(e *Entry) error {
	if e == nil {
		return nil
	}
	e.CustomerTitle = clampRunes(e.CustomerTitle, maxCustomerTitleRunes)
	e.CustomerRef = clampRunes(e.CustomerRef, maxCustomerRefRunes)
	e.CustomerUpdate = clampRunes(e.CustomerUpdate, maxCustomerUpdateRunes)
	e.ResolutionNote = clampRunes(e.ResolutionNote, maxResolutionNoteRunes)
	e.Severity = strings.ToLower(strings.TrimSpace(e.Severity))
	e.Phase = strings.ToLower(strings.TrimSpace(e.Phase))
	e.Resolution = strings.ToLower(strings.TrimSpace(e.Resolution))

	if e.Dossier != nil {
		raw, err := json.Marshal(e.Dossier)
		if err != nil {
			return fmt.Errorf("dossier marshal: %w", err)
		}
		if len(raw) > maxDossierJSONBytes {
			return fmt.Errorf("dossier too large (%d bytes; max %d)", len(raw), maxDossierJSONBytes)
		}
	}
	return nil
}

func clampRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	if max < 1 {
		return ""
	}
	return string(r[:max-1]) + "…"
}
