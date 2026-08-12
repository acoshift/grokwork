package apitoken

import "github.com/acoshift/grokwork/internal/config"

// CapsMask is the ceiling a token may exercise. Investigate is stored for
// forward-compat but ignored for v1 gating (K20).
type CapsMask struct {
	Investigate   bool `json:"investigate,omitempty"`
	StartSessions bool `json:"startSessions,omitempty"`
	GithubWrites  bool `json:"githubWrites,omitempty"`
}

// Intersect applies the token mask to a team's resolved capabilities.
// Merge / Approve / AdminProject / FileEscalation / SafeOps stay false.
func Intersect(c config.Capabilities, m CapsMask) config.Capabilities {
	return config.Capabilities{
		Investigate:   c.Investigate,
		StartSessions: c.StartSessions && m.StartSessions,
		GithubWrites:  c.GithubWrites && m.GithubWrites,
	}
}
