package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/acoshift/grokwork/internal/grokrun"
)

// ModelAllowed reports whether a new session may run on name. Empty is always
// allowed (the CLI picks). A name that is not on the denylist is allowed even
// when it is uncurated — RequestedAgentCLI still refuses uncurated names on its
// own, so this predicate does not have to.
func (c *Config) ModelAllowed(name string) bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.modelAllowedLocked(name)
}

func (c *Config) modelAllowedLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	return !slices.Contains(c.DisabledModels, name)
}

// DisabledModelNames is a copy of the denylist. Empty means none disabled.
func (c *Config) DisabledModelNames() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.DisabledModels)
}

// SetModelDisabled adds or removes one curated name. Unknown names are refused
// rather than stored: a typo in the denylist would look like a disable that
// never matches anything.
func (c *Config) SetModelDisabled(name string, disabled bool) error {
	name = strings.TrimSpace(name)
	if !grokrun.IsKnownModel(name) {
		return fmt.Errorf("model %q is not a known model", name)
	}
	return c.setNamesDisabled([]string{name}, disabled)
}

// SetAgentModelsDisabled expands to every curated name of that agent.
func (c *Config) SetAgentModelsDisabled(agent string, disabled bool) error {
	a, err := parseExplicitAgent(agent)
	if err != nil {
		return err
	}
	names := curatedNames(a, "")
	if len(names) == 0 {
		return fmt.Errorf("no curated models for %s", a.Label())
	}
	return c.setNamesDisabled(names, disabled)
}

// SetFamilyModelsDisabled expands to every curated name of that agent+family
// (e.g. Cursor GPT). Individual re-enable of one name after a bulk disable
// works because the store is the expanded list, not a parallel family flag.
func (c *Config) SetFamilyModelsDisabled(agent, family string, disabled bool) error {
	a, err := parseExplicitAgent(agent)
	if err != nil {
		return err
	}
	family = strings.ToLower(strings.TrimSpace(family))
	if family == "" {
		return fmt.Errorf("family is required")
	}
	names := curatedNames(a, family)
	if len(names) == 0 {
		return fmt.Errorf("no curated models for %s %s", a.Label(), grokrun.ModelFamilyLabel(family))
	}
	return c.setNamesDisabled(names, disabled)
}

// SetDisabledModels replaces the whole denylist and persists it.
func (c *Config) SetDisabledModels(names []string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	clean := normalizeDisabledModels(names)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(clean) == 0 {
		c.DisabledModels = nil
	} else {
		c.DisabledModels = clean
	}
	return c.saveLocked()
}

func (c *Config) setNamesDisabled(names []string, disabled bool) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := slices.Clone(c.DisabledModels)
	if disabled {
		cur = append(cur, names...)
	} else {
		drop := map[string]struct{}{}
		for _, n := range names {
			drop[n] = struct{}{}
		}
		kept := cur[:0]
		for _, n := range cur {
			if _, ok := drop[n]; !ok {
				kept = append(kept, n)
			}
		}
		cur = kept
	}
	clean := normalizeDisabledModels(cur)
	if len(clean) == 0 {
		c.DisabledModels = nil
	} else {
		c.DisabledModels = clean
	}
	return c.saveLocked()
}

func parseExplicitAgent(agent string) (grokrun.Agent, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return "", fmt.Errorf("agent is required")
	}
	a, ok := grokrun.ParseAgent(agent)
	if !ok {
		return "", fmt.Errorf("agent %q is not a known coding CLI (want %s)",
			agent, grokrun.KnownAgents())
	}
	return a, nil
}

func curatedNames(agent grokrun.Agent, family string) []string {
	var out []string
	for _, opt := range grokrun.ModelOptions() {
		if opt.Agent != agent {
			continue
		}
		if family != "" && grokrun.ModelFamily(opt.Value) != family {
			continue
		}
		out = append(out, opt.Value)
	}
	return out
}

func normalizeDisabledModels(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || !grokrun.IsKnownModel(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ModelAvailChoice is one curated model on the admin availability page.
type ModelAvailChoice struct {
	Value   string
	Label   string
	Enabled bool
	Agent   string
	Family  string
}

// ModelAvailFamily is one vendor family within an agent. Cursor has several
// (GPT, Composer, …); Grok and Claude each have one, and the page hides the
// extra heading when ShowFamilies is false on the parent group.
type ModelAvailFamily struct {
	Key     string
	Label   string
	Agent   string
	Choices []ModelAvailChoice
}

// ModelAvailGroup is one agent's block on the admin availability page.
type ModelAvailGroup struct {
	Agent        string
	Label        string
	ShowFamilies bool
	Families     []ModelAvailFamily
}

// ModelAvailability lists every curated model grouped by agent, then by family
// when that agent has more than one. Enabled is the inverse of the denylist.
func (c *Config) ModelAvailability() []ModelAvailGroup {
	var deny []string
	if c != nil {
		c.mu.RLock()
		deny = slices.Clone(c.DisabledModels)
		c.mu.RUnlock()
	}
	return modelAvailabilityFrom(deny)
}

func modelAvailabilityFrom(deny []string) []ModelAvailGroup {
	groups := []ModelAvailGroup{
		{Agent: grokrun.AgentGrok.String(), Label: grokrun.AgentGrok.Label()},
		{Agent: grokrun.AgentClaude.String(), Label: grokrun.AgentClaude.Label()},
		{Agent: grokrun.AgentCursor.String(), Label: grokrun.AgentCursor.Label()},
	}
	familyIndex := map[string]map[string]int{} // agent → family → index in Families
	for i := range groups {
		familyIndex[groups[i].Agent] = map[string]int{}
	}
	for _, opt := range grokrun.ModelOptions() {
		agent := opt.Agent.String()
		gi := -1
		for i := range groups {
			if groups[i].Agent == agent {
				gi = i
				break
			}
		}
		if gi < 0 {
			continue
		}
		fam := grokrun.ModelFamily(opt.Value)
		fi, ok := familyIndex[agent][fam]
		if !ok {
			fi = len(groups[gi].Families)
			familyIndex[agent][fam] = fi
			groups[gi].Families = append(groups[gi].Families, ModelAvailFamily{
				Key:   fam,
				Label: grokrun.ModelFamilyLabel(fam),
				Agent: agent,
			})
		}
		groups[gi].Families[fi].Choices = append(groups[gi].Families[fi].Choices, ModelAvailChoice{
			Value:   opt.Value,
			Label:   opt.Label,
			Enabled: !slices.Contains(deny, opt.Value),
			Agent:   agent,
			Family:  fam,
		})
	}
	out := make([]ModelAvailGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.Families) == 0 {
			continue
		}
		g.ShowFamilies = len(g.Families) > 1
		out = append(out, g)
	}
	return out
}
