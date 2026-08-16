// Package errsrc holds shared types for production error-source integrations.
// HTTP clients live in subpackages (deploys, sentry, gcperr). This package
// must not import those subpackages — a parent URL dispatcher would cycle.
package errsrc

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ProviderGCP     = "gcp"
	ProviderSentry  = "sentry"
	ProviderDeploys = "deploys"
)

// SampleMaxRunes is the MCP/web stack cap (same ceiling as Linear descriptions).
const SampleMaxRunes = 8000

// Group is the list-row view of one grouped production error.
type Group struct {
	Provider    string
	ID          string
	ShortID     string
	Title       string
	Culprit     string
	Status      string
	Level       string
	Count       int64
	UserCount   int64
	FirstSeen   time.Time `json:",omitzero"`
	LastSeen    time.Time `json:",omitzero"`
	URL         string
	Fingerprint string
	Location    string
	Resource    string
}

// Event is one occurrence pointer (no secrets).
type Event struct {
	Timestamp time.Time `json:",omitzero"`
	Message   string
	Culprit   string
	Extra     string
}

// GroupDetail is the list row plus a capped sample stack and recent events.
type GroupDetail struct {
	Group
	Sample string
	Recent []Event
}

// ListQuery is the provider-native list filter.
type ListQuery struct {
	Status    string
	Sort      string
	Limit     int
	Cursor    string
	Service   string
	TimeRange string
}

// ListResult is one page of groups.
type ListResult struct {
	Groups     []Group
	NextCursor string
}

// Ref is a parsed provider URL. Subpackages return this; they do not import each other.
type Ref struct {
	Provider    string
	ID          string
	ShortID     string
	Location    string
	Resource    string
	ProjectHint string
}

// CapSample trims and rune-caps a sample stack. Clients cap before returning
// to MCP or templates.
func CapSample(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) <= SampleMaxRunes {
		return s
	}
	i := 0
	for idx := range s {
		if i == SampleMaxRunes {
			return s[:idx]
		}
		i++
	}
	return s
}
