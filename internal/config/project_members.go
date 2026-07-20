package config

import (
	"fmt"
	"slices"
	"strings"
)

// projectHasAllowlist reports whether the project has any members or roles.
func projectHasAllowlist(pc ProjectConfig) bool {
	return len(pc.AllowedUserIDs) > 0 || len(pc.AllowedRoleIDs) > 0
}

// ProjectHasAllowlist reports whether the named project has any user or role members.
func (c *Config) ProjectHasAllowlist(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && projectHasAllowlist(pc)
}

// AccessAllowed reports whether userID (or any of roleIDs) may use Grok on project.
// Empty project allowlist is fail-closed (false). Unknown project is false.
func (c *Config) AccessAllowed(project, userID string, roleIDs []string) bool {
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
	if len(pc.AllowedRoleIDs) == 0 || len(roleIDs) == 0 {
		return false
	}
	roleSet := toSet(pc.AllowedRoleIDs)
	for _, r := range roleIDs {
		if _, ok := roleSet[r]; ok {
			return true
		}
	}
	return false
}

// UserOnAnyProject reports whether discordUserID appears on any project's user allowlist.
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
		if containsID(pc.AllowedUserIDs, id) {
			return true
		}
	}
	return false
}

// ProjectsVisibleTo returns project names the user may see in the web UI.
// Admins see all projects; others see only projects that list their Discord user ID.
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
		if containsID(c.Projects[n].AllowedUserIDs, id) {
			out = append(out, n)
		}
	}
	return out
}

// CanAccessProject reports whether the user may open a project in the web UI.
// Admins always can; others need their Discord user ID on the project list.
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
	return ok && containsID(pc.AllowedUserIDs, discordUserID)
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

// AddProjectAllowedRole adds a Discord role ID to a project's allowlist and persists.
func (c *Config) AddProjectAllowedRole(project, id string) error {
	project = strings.TrimSpace(project)
	id = strings.TrimSpace(id)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if id == "" {
		return fmt.Errorf("role id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if containsID(pc.AllowedRoleIDs, id) {
		return nil
	}
	pc.AllowedRoleIDs = append(slices.Clone(pc.AllowedRoleIDs), id)
	c.Projects[project] = pc
	return c.saveLocked()
}

// RemoveProjectAllowedRole removes a Discord role ID from a project's allowlist.
func (c *Config) RemoveProjectAllowedRole(project, id string) error {
	project = strings.TrimSpace(project)
	id = strings.TrimSpace(id)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	if id == "" {
		return fmt.Errorf("role id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("project %q not found", project)
	}
	if !containsID(pc.AllowedRoleIDs, id) {
		return fmt.Errorf("role %q not found on project %q", id, project)
	}
	pc.AllowedRoleIDs = removeString(pc.AllowedRoleIDs, id)
	c.Projects[project] = pc
	return c.saveLocked()
}

// MigrateGlobalAllowlistToProjects copies legacy root allowlists into projects
// that have no members yet, then clears the root lists. Returns how many projects
// were filled. Caller must not hold c.mu. Saves when changed.
func (c *Config) MigrateGlobalAllowlistToProjects() (int, error) {
	if c == nil {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.migrateGlobalAllowlistLocked()
}

func (c *Config) migrateGlobalAllowlistLocked() (int, error) {
	globalUsers := cleanIDList(c.AllowedUserIDs)
	globalRoles := cleanIDList(c.AllowedRoleIDs)
	if len(globalUsers) == 0 && len(globalRoles) == 0 {
		return 0, nil
	}
	n := 0
	for name, pc := range c.Projects {
		if projectHasAllowlist(pc) {
			continue
		}
		pc.AllowedUserIDs = slices.Clone(globalUsers)
		pc.AllowedRoleIDs = slices.Clone(globalRoles)
		c.Projects[name] = pc
		n++
	}
	c.AllowedUserIDs = nil
	c.AllowedRoleIDs = nil
	c.AllowedUsers = map[string]struct{}{}
	c.AllowedRoles = map[string]struct{}{}
	if err := c.saveLocked(); err != nil {
		return n, err
	}
	return n, nil
}
