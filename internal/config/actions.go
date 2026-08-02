package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// cloneProjectActions deep-copies Actions so Snapshot/save never share slices.
func cloneProjectActions(a *ProjectActionsConfig) *ProjectActionsConfig {
	if a == nil {
		return nil
	}
	cp := *a
	if len(a.DispatchRules) > 0 {
		cp.DispatchRules = make([]ActionsDispatchRule, len(a.DispatchRules))
		for i, r := range a.DispatchRules {
			cp.DispatchRules[i] = ActionsDispatchRule{
				Repo:     r.Repo,
				Workflow: r.Workflow,
				Branches: slices.Clone(r.Branches),
			}
		}
	} else {
		cp.DispatchRules = nil
	}
	return &cp
}

// normalizeProjectActions trims and drops empty rules at load time.
// Returns nil when nothing remains so config.json never keeps an empty object.
func normalizeProjectActions(a *ProjectActionsConfig) *ProjectActionsConfig {
	if a == nil {
		return nil
	}
	rules := make([]ActionsDispatchRule, 0, len(a.DispatchRules))
	for _, r := range a.DispatchRules {
		r.Repo = strings.TrimSpace(r.Repo)
		r.Workflow = strings.TrimSpace(r.Workflow)
		r.Branches = cleanIDList(r.Branches)
		if r.Workflow == "" || len(r.Branches) == 0 {
			continue
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return nil
	}
	a.DispatchRules = rules
	return a
}

// ActionsDispatchBranches returns the branches a workflow may be dispatched on,
// and whether a lock applies. locked==false means all branches are allowed.
//
// Most specific rule wins: a rule whose Repo matches repo or owner/repo
// (case-insensitive) beats a rule with Repo=="". Workflow is matched against
// filepath.Base(workflowPath), case-insensitively. The returned slice is a copy.
func (c *Config) ActionsDispatchBranches(project, owner, repo, workflowPath string) (branches []string, locked bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[project]
	if !ok || pc.Actions == nil || len(pc.Actions.DispatchRules) == 0 {
		return nil, false
	}
	wfBase := filepath.Base(strings.TrimSpace(workflowPath))
	if wfBase == "" || wfBase == "." {
		return nil, false
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	full := ""
	if owner != "" && repo != "" {
		full = owner + "/" + repo
	}

	var wild, exact []string
	var hasWild, hasExact bool
	for _, rule := range pc.Actions.DispatchRules {
		if !strings.EqualFold(strings.TrimSpace(rule.Workflow), wfBase) {
			continue
		}
		rRepo := strings.TrimSpace(rule.Repo)
		if rRepo == "" {
			hasWild = true
			wild = rule.Branches
			continue
		}
		if repo != "" && strings.EqualFold(rRepo, repo) {
			hasExact = true
			exact = rule.Branches
			continue
		}
		if full != "" && strings.EqualFold(rRepo, full) {
			hasExact = true
			exact = rule.Branches
		}
	}
	if hasExact {
		return slices.Clone(exact), true
	}
	if hasWild {
		return slices.Clone(wild), true
	}
	return nil, false
}

// SetProjectActionsRule adds or replaces a dispatch branch-lock rule for a
// project. The key is (repo, workflow) case-insensitive; empty repo is the
// project-wide wildcard. Zero rules after normalize clears Actions to nil.
func (c *Config) SetProjectActionsRule(name string, rule ActionsDispatchRule) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	rule.Repo = strings.TrimSpace(rule.Repo)
	rule.Workflow = strings.TrimSpace(rule.Workflow)
	rule.Branches = cleanIDList(rule.Branches)
	if rule.Workflow == "" {
		return fmt.Errorf("workflow file is required")
	}
	if len(rule.Branches) == 0 {
		return fmt.Errorf("at least one branch is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	act := cloneProjectActions(pc.Actions)
	if act == nil {
		act = &ProjectActionsConfig{}
	}
	replaced := false
	for i, existing := range act.DispatchRules {
		if strings.EqualFold(existing.Workflow, rule.Workflow) &&
			strings.EqualFold(strings.TrimSpace(existing.Repo), rule.Repo) {
			act.DispatchRules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		act.DispatchRules = append(act.DispatchRules, rule)
	}
	pc.Actions = normalizeProjectActions(act)
	c.Projects[name] = pc
	return c.saveLocked()
}

// RemoveProjectActionsRule drops the rule matching (repo, workflow).
// Absent is not an error (idempotent). Clearing the last rule sets Actions nil.
func (c *Config) RemoveProjectActionsRule(name, repo, workflow string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	repo = strings.TrimSpace(repo)
	workflow = strings.TrimSpace(workflow)
	if workflow == "" {
		return fmt.Errorf("workflow file is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	if pc.Actions == nil || len(pc.Actions.DispatchRules) == 0 {
		pc.Actions = nil
		c.Projects[name] = pc
		return c.saveLocked()
	}
	act := cloneProjectActions(pc.Actions)
	kept := make([]ActionsDispatchRule, 0, len(act.DispatchRules))
	for _, r := range act.DispatchRules {
		if strings.EqualFold(r.Workflow, workflow) &&
			strings.EqualFold(strings.TrimSpace(r.Repo), repo) {
			continue
		}
		kept = append(kept, r)
	}
	act.DispatchRules = kept
	pc.Actions = normalizeProjectActions(act)
	c.Projects[name] = pc
	return c.saveLocked()
}
