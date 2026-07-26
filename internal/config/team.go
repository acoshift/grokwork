package config

import (
	"fmt"
	"slices"
	"strings"
)

// TeamConfig is one per-project team. Membership grants BOTH project access and
// the capabilities of the named template — one concept replacing allowedRoleIds
// (access) and capabilityByRole (capabilities).
//
// Members are namespaced actor ids (see NormalizeActorID): a bare snowflake
// still means Discord, so a team can name an OIDC user without a second
// migration later.
type TeamConfig struct {
	// Label is the human-readable name ("Support"). The map key is the id.
	Label string `json:"label,omitempty"`
	// Members are normalized actor ids ("discord:123", "oidc:alice").
	Members []string `json:"members,omitempty"`
	// Capabilities names a capability template (builtin or a project overlay).
	// Empty grants access only — capabilities then come from the unmapped
	// default, exactly like an allowedUserIds-only member.
	Capabilities string `json:"capabilities,omitempty"`
}

// cloneTeams deep-copies a team map, including each Members slice. A shallow
// copy would let a Snapshot and the live config share the same backing array,
// so appending a member would mutate a value another goroutine is reading.
func cloneTeams(m map[string]TeamConfig) map[string]TeamConfig {
	if m == nil {
		return nil
	}
	out := make(map[string]TeamConfig, len(m))
	for k, v := range m {
		v.Members = slices.Clone(v.Members)
		out[k] = v
	}
	return out
}

// normalizeTeamKey canonicalizes a team map key. Keys are ids, not labels, so
// they are case-insensitive; the display name lives in Label.
func normalizeTeamKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// cleanActorList normalizes a member list: every id namespaced, empties dropped,
// duplicates collapsed. Dedupe is on the normalized form so "123" and
// "discord:123" cannot both be stored for the same person.
func cleanActorList(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		n := NormalizeActorID(id)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeTeams canonicalizes keys, members and template names. An entry whose
// key is empty is dropped — a team with no id cannot be addressed by any route.
func normalizeTeams(m map[string]TeamConfig) map[string]TeamConfig {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]TeamConfig, len(m))
	for k, v := range m {
		key := normalizeTeamKey(k)
		if key == "" {
			continue
		}
		v.Label = strings.TrimSpace(v.Label)
		v.Members = cleanActorList(v.Members)
		v.Capabilities = strings.ToLower(strings.TrimSpace(v.Capabilities))
		out[key] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// teamsContainActor reports whether any team of pc names actorID.
func teamsContainActor(pc ProjectConfig, actorID string) bool {
	if strings.TrimSpace(actorID) == "" {
		return false
	}
	for _, t := range pc.Teams {
		if containsID(t.Members, actorID) {
			return true
		}
	}
	return false
}

// projectTeamsHaveMembers reports whether any team actually names someone.
// A team declared with an empty members list grants nothing, so it must not
// satisfy the fail-closed allowlist check.
func projectTeamsHaveMembers(pc ProjectConfig) bool {
	for _, t := range pc.Teams {
		if len(t.Members) > 0 {
			return true
		}
	}
	return false
}

// teamsForActor returns the teams actorID belongs to, in sorted-key order.
// Iterating the map directly would make the OR-merge order (and anything else
// derived from it) depend on Go's randomized map iteration.
func teamsForActor(pc ProjectConfig, actorID string) []TeamConfig {
	if strings.TrimSpace(actorID) == "" || len(pc.Teams) == 0 {
		return nil
	}
	keys := make([]string, 0, len(pc.Teams))
	for k := range pc.Teams {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []TeamConfig
	for _, k := range keys {
		t := pc.Teams[k]
		if containsID(t.Members, actorID) {
			out = append(out, t)
		}
	}
	return out
}

// projectActorIDsLocked returns the union of allowedUserIds and every team's
// members, normalized, sorted and deduped — "who is on this project".
// Caller holds c.mu.
func projectActorIDsLocked(pc ProjectConfig) []string {
	seen := make(map[string]struct{})
	add := func(id string) {
		if n := NormalizeActorID(id); n != "" {
			seen[n] = struct{}{}
		}
	}
	for _, id := range pc.AllowedUserIDs {
		add(id)
	}
	for _, t := range pc.Teams {
		for _, id := range t.Members {
			add(id)
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// ProjectActorIDs returns every actor with access to the project: allowedUserIds
// plus every team member, normalized and sorted.
func (c *Config) ProjectActorIDs(project string) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return nil
	}
	return projectActorIDsLocked(pc)
}

// ProjectTeams returns a copy of the project's teams.
func (c *Config) ProjectTeams(project string) map[string]TeamConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return nil
	}
	return cloneTeams(pc.Teams)
}

// SetProjectTeam creates or updates a team and persists. An existing team keeps
// its members — only the label and capability template are replaced.
// capabilities empty makes it an access-only team (capabilities then come from
// the project's unmapped default); a non-empty value must name a known template.
func (c *Config) SetProjectTeam(project, key, label, capabilities string) error {
	project = strings.TrimSpace(project)
	key = normalizeTeamKey(key)
	label = strings.TrimSpace(label)
	capabilities = strings.ToLower(strings.TrimSpace(capabilities))
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if key == "" {
		return fmt.Errorf("team key is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if capabilities != "" {
		if _, ok := lookupTemplate(capabilities, pc.CapabilityTemplates); !ok {
			return fmt.Errorf("unknown capability template %q", capabilities)
		}
	}
	teams := cloneTeams(pc.Teams)
	if teams == nil {
		teams = make(map[string]TeamConfig, 1)
	}
	t := teams[key]
	t.Label = label
	t.Capabilities = capabilities
	teams[key] = t
	pc.Teams = teams
	c.Projects[project] = pc
	return c.saveLocked()
}

// RemoveProjectTeam deletes a team (and therefore every grant it carried).
func (c *Config) RemoveProjectTeam(project, key string) error {
	project = strings.TrimSpace(project)
	key = normalizeTeamKey(key)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if key == "" {
		return fmt.Errorf("team key is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if _, hit := pc.Teams[key]; !hit {
		return fmt.Errorf("team %q not found on project %q", key, project)
	}
	teams := cloneTeams(pc.Teams)
	delete(teams, key)
	if len(teams) == 0 {
		teams = nil
	}
	pc.Teams = teams
	c.Projects[project] = pc
	return c.saveLocked()
}

// AddProjectTeamMember adds an actor to a team and persists. The id is
// normalized, so "123" and "discord:123" are the same member. Already a member
// is a no-op (matches AddProjectAllowedUser).
func (c *Config) AddProjectTeamMember(project, key, actorID string) error {
	project = strings.TrimSpace(project)
	key = normalizeTeamKey(key)
	id := NormalizeActorID(actorID)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if key == "" {
		return fmt.Errorf("team key is required")
	}
	if id == "" {
		return fmt.Errorf("actor id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	t, hit := pc.Teams[key]
	if !hit {
		return fmt.Errorf("team %q not found on project %q", key, project)
	}
	if containsID(t.Members, id) {
		return nil
	}
	teams := cloneTeams(pc.Teams)
	t = teams[key]
	t.Members = append(t.Members, id)
	teams[key] = t
	pc.Teams = teams
	c.Projects[project] = pc
	return c.saveLocked()
}

// RemoveProjectTeamMember removes an actor from a team. Absent is an error
// (mirrors RemoveProjectAllowedUser) so a typo is not reported as success.
func (c *Config) RemoveProjectTeamMember(project, key, actorID string) error {
	project = strings.TrimSpace(project)
	key = normalizeTeamKey(key)
	id := NormalizeActorID(actorID)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if key == "" {
		return fmt.Errorf("team key is required")
	}
	if id == "" {
		return fmt.Errorf("actor id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	t, hit := pc.Teams[key]
	if !hit {
		return fmt.Errorf("team %q not found on project %q", key, project)
	}
	if !containsID(t.Members, id) {
		return fmt.Errorf("actor %q not on team %q of project %q", id, key, project)
	}
	teams := cloneTeams(pc.Teams)
	t = teams[key]
	t.Members = removeActorID(t.Members, id)
	if len(t.Members) == 0 {
		t.Members = nil
	}
	teams[key] = t
	pc.Teams = teams
	c.Projects[project] = pc
	return c.saveLocked()
}

// removeActorID drops every entry denoting the same actor as want, comparing
// normalized ids so a hand-written bare id is removed by a namespaced request.
func removeActorID(ids []string, want string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if SameActor(id, want) {
			continue
		}
		out = append(out, id)
	}
	return out
}
