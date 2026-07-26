package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SLASeverities is the case severity vocabulary an SLA table may be keyed by,
// most urgent first. It is also the render order of the settings form.
//
// Severities themselves belong to a case (sessionstore.Entry.Severity) and are
// normalized at intake by bot.normalizeSeverity; config cannot import either, so
// TestSeverityVocabularyMatchesIntake pins the two lists together instead. A
// severity intake can produce but config has never heard of would be a severity
// nobody can write an SLA for.
var SLASeverities = []string{"critical", "high", "medium", "low"}

// SLATarget is one severity's two clocks, in minutes.
//
// Pointers, not ints: "this project has never configured an SLA" has to stay
// distinguishable from a configured value, or every case in every project would
// read as due immediately. A non-positive value means no target as well (see
// ProjectSLA) — a zero-minute deadline is not a policy — but it still
// round-trips as an explicit zero rather than disappearing on the next save.
type SLATarget struct {
	// FirstResponseMinutes is how long the customer may wait for the first
	// customer-facing reply of a round.
	FirstResponseMinutes *int `json:"firstResponseMinutes,omitempty"`
	// ResolutionMinutes is how long the case itself may stay open. The clock
	// pauses while the case is waiting on the customer — see
	// internal/bot/case_sla.go, which owns that decision.
	ResolutionMinutes *int `json:"resolutionMinutes,omitempty"`
}

// normalizeSLA lowercases the severity keys and refuses one that is not a
// severity.
//
// A typo is a hard parse error rather than a dropped row for the same reason a
// deploy.yaml typo is: an SLA silently keyed to "sev1" or "hgih" applies to no
// case at all, and the failure is invisible — the board simply never badges
// anything, which looks exactly like nothing being late.
func normalizeSLA(in map[string]SLATarget) (map[string]SLATarget, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]SLATarget, len(in))
	for k, v := range in {
		key := strings.ToLower(strings.TrimSpace(k))
		if !slices.Contains(SLASeverities, key) {
			return nil, fmt.Errorf("sla[%q]: unknown severity (want one of %s)", k, strings.Join(SLASeverities, ", "))
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("sla[%q]: duplicate severity", k)
		}
		out[key] = v
	}
	return out, nil
}

func cloneSLA(in map[string]SLATarget) map[string]SLATarget {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]SLATarget, len(in))
	for k, v := range in {
		out[k] = SLATarget{
			FirstResponseMinutes: cloneIntPtr(v.FirstResponseMinutes),
			ResolutionMinutes:    cloneIntPtr(v.ResolutionMinutes),
		}
	}
	return out
}

// ProjectSLA returns one severity's targets as durations. Zero means "no
// target": unset, non-positive, an unknown severity, or a project with no SLA
// table at all. Callers must treat zero as "this clock does not exist" and never
// as a deadline that has already passed.
func (c *Config) ProjectSLA(project, severity string) (firstResponse, resolution time.Duration) {
	if c == nil {
		return 0, 0
	}
	sev := strings.ToLower(strings.TrimSpace(severity))
	if sev == "" {
		return 0, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return 0, 0
	}
	t, ok := pc.SLA[sev]
	if !ok {
		return 0, 0
	}
	return slaMinutes(t.FirstResponseMinutes), slaMinutes(t.ResolutionMinutes)
}

func slaMinutes(m *int) time.Duration {
	if m == nil || *m <= 0 {
		return 0
	}
	return time.Duration(*m) * time.Minute
}

// SetProjectSLA replaces a project's SLA table and persists it. An empty table
// clears every target, which is how a project opts back out of SLAs entirely.
func (c *Config) SetProjectSLA(name string, targets map[string]SLATarget) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	clean, err := normalizeSLA(targets)
	if err != nil {
		return err
	}
	for sev, t := range clean {
		if t.FirstResponseMinutes != nil && *t.FirstResponseMinutes < 0 {
			return fmt.Errorf("sla[%q]: first response minutes must be >= 0", sev)
		}
		if t.ResolutionMinutes != nil && *t.ResolutionMinutes < 0 {
			return fmt.Errorf("sla[%q]: resolution minutes must be >= 0", sev)
		}
		if t.FirstResponseMinutes == nil && t.ResolutionMinutes == nil {
			// Both cleared: drop the row rather than persisting an empty object.
			delete(clean, sev)
		}
	}
	if len(clean) == 0 {
		clean = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("unknown project %q", name)
	}
	pc.SLA = clean
	c.Projects[name] = pc
	return c.saveLocked()
}

// SLAItem is one severity row of the settings form. The minutes are strings so
// an empty box round-trips as "no target" instead of as zero — the same reason
// SLATarget uses pointers.
type SLAItem struct {
	Severity      string
	FirstResponse string
	Resolution    string
}

// slaItems renders a project's table as form rows, one per known severity in
// SLASeverities order (so the form always shows every severity, configured or
// not).
func slaItems(in map[string]SLATarget) []SLAItem {
	out := make([]SLAItem, 0, len(SLASeverities))
	for _, sev := range SLASeverities {
		row := SLAItem{Severity: sev}
		if t, ok := in[sev]; ok {
			row.FirstResponse = minutesField(t.FirstResponseMinutes)
			row.Resolution = minutesField(t.ResolutionMinutes)
		}
		out = append(out, row)
	}
	return out
}

func minutesField(m *int) string {
	if m == nil {
		return ""
	}
	return strconv.Itoa(*m)
}

// ParseSLAMinutes reads one form field. An empty field is "no target" (nil), not
// zero, so clearing a box removes the deadline instead of setting an impossible
// one.
func ParseSLAMinutes(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number of minutes", raw)
	}
	if n < 0 {
		return nil, fmt.Errorf("minutes must be >= 0")
	}
	return &n, nil
}

// slaConfigured reports whether any severity has a usable target, so the
// settings page can say "no SLA configured" once instead of four times. Takes
// the table rather than a project name: the snapshot builder calls it with the
// config lock already held.
func slaConfigured(in map[string]SLATarget) bool {
	for _, t := range in {
		if slaMinutes(t.FirstResponseMinutes) > 0 || slaMinutes(t.ResolutionMinutes) > 0 {
			return true
		}
	}
	return false
}
