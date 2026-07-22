package config

import (
	"strings"
)

// Capabilities are project-scoped action flags (fail-closed when zero).
// Wave 1: RequestChange/SafeOps reserved (no command gates).
type Capabilities struct {
	Investigate        bool `json:"investigate,omitempty"`
	DraftCustomerReply bool `json:"draftCustomerReply,omitempty"`
	FileEscalation     bool `json:"fileEscalation,omitempty"`
	RequestChange      bool `json:"requestChange,omitempty"`
	SafeOps            bool `json:"safeOps,omitempty"`
	StartSessions      bool `json:"startSessions,omitempty"`
	GithubWrites       bool `json:"githubWrites,omitempty"`
	Merge              bool `json:"merge,omitempty"`
	Approve            bool `json:"approve,omitempty"`
	AdminProject       bool `json:"adminProject,omitempty"`
}

// BuiltinCapabilityTemplates are always available template names.
var BuiltinCapabilityTemplates = map[string]Capabilities{
	"investigator": {
		Investigate: true, DraftCustomerReply: true, FileEscalation: true,
	},
	"operator": {
		Investigate: true, DraftCustomerReply: true,
	},
	"builder": {
		Investigate: true, StartSessions: true, GithubWrites: true,
	},
	"approver": {
		Investigate: true, DraftCustomerReply: true, FileEscalation: true,
		StartSessions: true, GithubWrites: true, Approve: true,
	},
	"admin": {
		Investigate: true, DraftCustomerReply: true, FileEscalation: true,
		StartSessions: true, GithubWrites: true, Merge: true, Approve: true,
		AdminProject: true,
	},
}

// Or merges bool flags (any true wins).
func (c Capabilities) Or(o Capabilities) Capabilities {
	return Capabilities{
		Investigate:        c.Investigate || o.Investigate,
		DraftCustomerReply: c.DraftCustomerReply || o.DraftCustomerReply,
		FileEscalation:     c.FileEscalation || o.FileEscalation,
		RequestChange:      c.RequestChange || o.RequestChange,
		SafeOps:            c.SafeOps || o.SafeOps,
		StartSessions:      c.StartSessions || o.StartSessions,
		GithubWrites:       c.GithubWrites || o.GithubWrites,
		Merge:              c.Merge || o.Merge,
		Approve:            c.Approve || o.Approve,
		AdminProject:       c.AdminProject || o.AdminProject,
	}
}

// CanShip is true when both start and github write flags are set (builder-class).
func (c Capabilities) CanShip() bool {
	return c.StartSessions && c.GithubWrites
}

// TemplateName lookup: project overlay then builtin. Unknown → zero + false.
func (c *Config) ResolveTemplate(project, name string) (Capabilities, bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return Capabilities{}, false
	}
	if c != nil {
		c.mu.RLock()
		pc, ok := c.Projects[project]
		c.mu.RUnlock()
		if ok && pc.CapabilityTemplates != nil {
			if caps, hit := pc.CapabilityTemplates[name]; hit {
				return caps, true
			}
			// also try original case keys
			for k, v := range pc.CapabilityTemplates {
				if strings.EqualFold(k, name) {
					return v, true
				}
			}
		}
	}
	if caps, ok := BuiltinCapabilityTemplates[name]; ok {
		return caps, true
	}
	return Capabilities{}, false
}

// SafeTeamMode reports whether the project has safeTeamMode enabled.
func (c *Config) SafeTeamMode(project string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok || pc.SafeTeamMode == nil {
		return false
	}
	return *pc.SafeTeamMode
}

// SafeTeamDefaultTemplate returns the unmapped template name (default investigator).
func (c *Config) SafeTeamDefaultTemplate(project string) string {
	if c == nil {
		return "investigator"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return "investigator"
	}
	t := strings.TrimSpace(pc.SafeTeamDefaultTemplate)
	if t == "" {
		return "investigator"
	}
	return strings.ToLower(t)
}

// ProjectDefaultMode returns projects.*.defaultMode (empty = legacy fix).
func (c *Config) ProjectDefaultMode(project string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(pc.DefaultMode))
}

// ProjectInvestigateTools returns the investigate tools allowlist (empty = default).
func (c *Config) ProjectInvestigateTools(project string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return ""
	}
	return strings.TrimSpace(pc.InvestigateTools)
}

// ResolveCapabilities maps membership + templates → flags (K12 Discord path).
// Caller must already have passed AccessAllowed.
// SafeTeamMode off → builder default for unmapped (backward compat).
// SafeTeamMode on → unmapped → safeTeamDefaultTemplate (default investigator) (K16).
func (c *Config) ResolveCapabilities(project, userID string, roleIDs []string) Capabilities {
	if c == nil {
		return BuiltinCapabilityTemplates["builder"]
	}
	c.mu.RLock()
	pc, ok := c.Projects[project]
	safe := pc.SafeTeamMode != nil && *pc.SafeTeamMode
	defaultTpl := strings.TrimSpace(pc.SafeTeamDefaultTemplate)
	byUser := pc.CapabilityByUser
	byRole := pc.CapabilityByRole
	overlays := pc.CapabilityTemplates
	c.mu.RUnlock()
	if !ok {
		return Capabilities{}
	}
	if defaultTpl == "" {
		defaultTpl = "investigator"
	}

	var caps Capabilities
	var any bool
	uid := strings.TrimSpace(userID)
	if uid != "" && byUser != nil {
		if name, hit := byUser[uid]; hit {
			if t, ok := lookupTemplate(name, overlays); ok {
				caps = caps.Or(t)
				any = true
			}
		}
	}
	for _, rid := range roleIDs {
		rid = strings.TrimSpace(rid)
		if rid == "" || byRole == nil {
			continue
		}
		if name, hit := byRole[rid]; hit {
			if t, ok := lookupTemplate(name, overlays); ok {
				caps = caps.Or(t)
				any = true
			}
		}
	}
	if any {
		return caps
	}
	// Unmapped
	if safe {
		t, ok := lookupTemplate(defaultTpl, overlays)
		if !ok {
			return Capabilities{} // unknown template → fail closed
		}
		return t
	}
	return BuiltinCapabilityTemplates["builder"]
}

func lookupTemplate(name string, overlays map[string]Capabilities) (Capabilities, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Capabilities{}, false
	}
	if overlays != nil {
		if caps, ok := overlays[name]; ok {
			return caps, true
		}
		for k, v := range overlays {
			if strings.EqualFold(k, name) {
				return v, true
			}
		}
	}
	key := strings.ToLower(name)
	if caps, ok := BuiltinCapabilityTemplates[key]; ok {
		return caps, true
	}
	return Capabilities{}, false
}

// MaxConcurrentRunsValue returns host-wide concurrent run cap (nil → 0 = unlimited).
func (c *Config) MaxConcurrentRunsValue() int {
	if c == nil || c.MaxConcurrentRuns == nil || *c.MaxConcurrentRuns <= 0 {
		return 0
	}
	return *c.MaxConcurrentRuns
}

// MaxConcurrentRunsUserValue returns per-user concurrent run cap (nil → 0 = unlimited).
func (c *Config) MaxConcurrentRunsUserValue() int {
	if c == nil || c.MaxConcurrentRunsUser == nil || *c.MaxConcurrentRunsUser <= 0 {
		return 0
	}
	return *c.MaxConcurrentRunsUser
}

// GrokEnvDenylistPrefixes returns configured denylist prefixes (plus built-in defaults always applied in grokrun).
func (c *Config) GrokEnvDenylistPrefixes() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slicesClone(c.GrokEnvDenylist)
}

func slicesClone(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// cloneCapabilitiesMap deep-copies a template map.
func cloneCapabilitiesMap(m map[string]Capabilities) map[string]Capabilities {
	if m == nil {
		return nil
	}
	out := make(map[string]Capabilities, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}


