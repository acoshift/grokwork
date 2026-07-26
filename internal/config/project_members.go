package config

import (
	"fmt"
	"slices"
	"strings"
)

// projectHasAllowlist reports whether the project grants access to anyone: a
// direct member, or a team that actually names someone. A team declared with an
// empty members list is not a grant — it must stay fail-closed.
func projectHasAllowlist(pc ProjectConfig) bool {
	return len(pc.AllowedUserIDs) > 0 || projectTeamsHaveMembers(pc)
}

// ProjectHasAllowlist reports whether the named project grants access to anyone.
func (c *Config) ProjectHasAllowlist(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && projectHasAllowlist(pc)
}

// AccessAllowed reports whether userID may use Grok on project. Access comes
// from a direct allowedUserIds entry or from membership of one of the project's
// teams. Empty project allowlist is fail-closed (false). Unknown project is false.
func (c *Config) AccessAllowed(project, userID string) bool {
	if c == nil || strings.TrimSpace(project) == "" || strings.TrimSpace(userID) == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok || !projectHasAllowlist(pc) {
		return false
	}
	if containsID(pc.AllowedUserIDs, userID) {
		return true
	}
	return teamsContainActor(pc, userID)
}

// UserOnAnyProject reports whether the actor is a direct member or a team member
// of any project.
func (c *Config) UserOnAnyProject(discordUserID string) bool {
	if c == nil {
		return false
	}
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, pc := range c.Projects {
		if containsID(pc.AllowedUserIDs, id) || teamsContainActor(pc, id) {
			return true
		}
	}
	return false
}

// ProjectsVisibleTo returns project names the user may see in the web UI.
// Admins see all projects; others see projects that list them directly or
// through a team.
func (c *Config) ProjectsVisibleTo(discordUserID string, role WebRole) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	slices.Sort(names)
	if RoleAtLeast(role, WebRoleAdmin) {
		return names
	}
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		pc := c.Projects[n]
		if containsID(pc.AllowedUserIDs, id) || teamsContainActor(pc, id) {
			out = append(out, n)
		}
	}
	return out
}

// CanAccessProject reports whether the user may open a project in the web UI.
// Admins always can; others need to be a direct member or on one of its teams.
func (c *Config) CanAccessProject(project, discordUserID string, role WebRole) bool {
	if c == nil {
		return false
	}
	if RoleAtLeast(role, WebRoleAdmin) {
		c.mu.RLock()
		_, ok := c.Projects[project]
		c.mu.RUnlock()
		return ok
	}
	if strings.TrimSpace(discordUserID) == "" || strings.TrimSpace(project) == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok {
		return false
	}
	return containsID(pc.AllowedUserIDs, discordUserID) || teamsContainActor(pc, discordUserID)
}

// AddProjectAllowedUser adds a Discord user ID to a project's allowlist and persists.
func (c *Config) AddProjectAllowedUser(project, id string) error {
	project = strings.TrimSpace(project)
	id = strings.TrimSpace(id)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if id == "" {
		return fmt.Errorf("user id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if containsID(pc.AllowedUserIDs, id) {
		return nil
	}
	pc.AllowedUserIDs = append(slices.Clone(pc.AllowedUserIDs), id)
	c.Projects[project] = pc
	return c.saveLocked()
}

// RemoveProjectAllowedUser removes a Discord user ID from a project's allowlist.
func (c *Config) RemoveProjectAllowedUser(project, id string) error {
	project = strings.TrimSpace(project)
	id = strings.TrimSpace(id)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if id == "" {
		return fmt.Errorf("user id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if !containsID(pc.AllowedUserIDs, id) {
		return fmt.Errorf("user %q not found on project %q", id, project)
	}
	pc.AllowedUserIDs = removeString(pc.AllowedUserIDs, id)
	c.Projects[project] = pc
	return c.saveLocked()
}
