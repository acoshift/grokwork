package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Deploy policy defaults.
const (
	// DefaultDeployStepTimeoutMaxMs caps what a manifest may request per step.
	// A project may lower it per environment; it may not raise it.
	DefaultDeployStepTimeoutMaxMs = 3_600_000
	// DefaultMaxConcurrentDeploys bounds host-wide deploy concurrency.
	DefaultMaxConcurrentDeploys = 4
	// DefaultDeployRunRetention is how many terminal runs are kept per lane.
	DefaultDeployRunRetention = 50
)

// ReservedDeployEnvPrefixes are env var names the runner injects itself. A
// manifest or project config that could set them would be able to forge the
// identity a step sees (which project, which actor, which SHA).
var ReservedDeployEnvPrefixes = []string{"GW_", "GROK_WORK_"}

// deployEnvKeyRe matches a POSIX-ish environment variable name.
var deployEnvKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deployEnvNameRe matches a deploy environment name.
//
// Deliberately a copy of internal/deploy's envNameRe rather than an import:
// internal/deploy will need this package for per-environment credentials, so a
// dependency the other way would be a cycle. The two are pinned to the same
// source string by DeployEnvNameRule and a test in each package.
var deployEnvNameRe = regexp.MustCompile(DeployEnvNameRule)

// DeployEnvNameRule is the shared environment-name rule. Names appear in the
// repo manifest, in on-disk lane paths, and in env var values, so they stay
// lowercase and free of separators.
const DeployEnvNameRule = `^[a-z][a-z0-9-]{0,31}$`

// ValidateDeployEnvName rejects a name that could never match a manifest, or
// that would be unsafe as a path segment.
func ValidateDeployEnvName(env string) error {
	env = strings.TrimSpace(env)
	if env == "" {
		return fmt.Errorf("environment name is required")
	}
	if !deployEnvNameRe.MatchString(env) {
		return fmt.Errorf("invalid environment name %q (lowercase letters, digits and dashes; must match the repo manifest)", env)
	}
	return nil
}

// ValidDeployCapabilities are the capability names an environment may demand.
// Deliberately a curated subset: these are the flags that already mean "trusted
// beyond ordinary builder" elsewhere in the product.
var ValidDeployCapabilities = []string{"approve", "adminProject", "safeOps", "merge"}

// DeployEnvConfig is per-environment deploy policy and credentials for one
// project. The pipeline itself lives in the repo; this holds only what must not
// be committed and what differs per host.
type DeployEnvConfig struct {
	// RequireCapability names a Capabilities flag the actor must hold. Empty
	// means builder-class (CanShip), matching every other money/risk gate.
	RequireCapability string `json:"requireCapability,omitempty"`
	// AllowedRefs restricts which git refs may be deployed here. Empty means
	// the project primary branch only — this is the control that stops a pushed
	// branch rewriting the manifest and reaching production credentials.
	// "*" opts an environment out (sensible for dev).
	AllowedRefs []string `json:"allowedRefs,omitempty"`
	// Env is injected into every step for this environment, at the highest
	// precedence. Values are secrets unless listed as non-secret; never log,
	// never expose through Snapshot, never send to Discord.
	Env map[string]string `json:"env,omitempty"`
	// SecretKeys marks which Env keys the log redactor scrubs. Secrecy is
	// marked rather than inferred from value length: the same map holds
	// non-secret config like K8S_NAMESPACE, and redacting that makes a failed
	// deploy's log unreadable exactly when it matters.
	SecretKeys []string `json:"secretKeys,omitempty"`
	// StepTimeoutMaxMs lowers the per-step ceiling for this environment.
	// nil → DefaultDeployStepTimeoutMaxMs.
	StepTimeoutMaxMs *int `json:"stepTimeoutMaxMs,omitempty"`
}

// ProjectDeployConfig is per-project deploy policy.
type ProjectDeployConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// ManifestPath overrides the in-repo manifest location.
	ManifestPath string                      `json:"manifestPath,omitempty"`
	Environments map[string]*DeployEnvConfig `json:"environments,omitempty"`
}

// IsSecretKey reports whether a key's value must be redacted from logs.
func (e *DeployEnvConfig) IsSecretKey(key string) bool {
	if e == nil {
		return false
	}
	return slices.Contains(e.SecretKeys, key)
}

// StepTimeoutMaxMsValue returns the effective per-step ceiling.
func (e *DeployEnvConfig) StepTimeoutMaxMsValue() int {
	if e == nil || e.StepTimeoutMaxMs == nil || *e.StepTimeoutMaxMs <= 0 {
		return DefaultDeployStepTimeoutMaxMs
	}
	return *e.StepTimeoutMaxMs
}

// SecretValues returns the values the log redactor must scrub. Callers must not
// log, render, or forward the result.
func (e *DeployEnvConfig) SecretValues() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.SecretKeys))
	for _, k := range e.SecretKeys {
		if v := e.Env[k]; v != "" {
			out = append(out, v)
		}
	}
	return out
}

func cloneDeployEnv(e *DeployEnvConfig) *DeployEnvConfig {
	if e == nil {
		return nil
	}
	cp := *e
	cp.AllowedRefs = slices.Clone(e.AllowedRefs)
	cp.SecretKeys = slices.Clone(e.SecretKeys)
	cp.Env = cloneStringMap(e.Env)
	cp.StepTimeoutMaxMs = cloneIntPtr(e.StepTimeoutMaxMs)
	return &cp
}

func cloneProjectDeploy(d *ProjectDeployConfig) *ProjectDeployConfig {
	if d == nil {
		return nil
	}
	cp := *d
	if len(d.Environments) > 0 {
		cp.Environments = make(map[string]*DeployEnvConfig, len(d.Environments))
		for k, v := range d.Environments {
			cp.Environments[k] = cloneDeployEnv(v)
		}
	} else {
		cp.Environments = nil
	}
	return &cp
}

// normalizeProjectDeploy trims and drops empties at load time. Returns nil when
// nothing is left, so config.json never grows an empty object.
func normalizeProjectDeploy(d *ProjectDeployConfig) *ProjectDeployConfig {
	if d == nil {
		return nil
	}
	d.ManifestPath = strings.TrimSpace(d.ManifestPath)
	for name, env := range d.Environments {
		if env == nil {
			delete(d.Environments, name)
			continue
		}
		env.RequireCapability = strings.TrimSpace(env.RequireCapability)
		env.AllowedRefs = cleanIDList(env.AllowedRefs)
		env.SecretKeys = cleanIDList(env.SecretKeys)
		// A SecretKeys entry with no matching Env key is stale bookkeeping.
		env.SecretKeys = slices.DeleteFunc(env.SecretKeys, func(k string) bool {
			_, ok := env.Env[k]
			return !ok
		})
	}
	if len(d.Environments) == 0 {
		d.Environments = nil
	}
	if !d.Enabled && d.ManifestPath == "" && len(d.Environments) == 0 {
		return nil
	}
	return d
}

// ValidateDeployEnvKey rejects names that are not env vars, and names the runner
// reserves for itself.
func ValidateDeployEnvKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("variable name is required")
	}
	if !deployEnvKeyRe.MatchString(key) {
		return fmt.Errorf("invalid variable name %q (letters, digits, underscore; not starting with a digit)", key)
	}
	for _, p := range ReservedDeployEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return fmt.Errorf("%q is reserved: the deploy runner sets %s* itself", key, p)
		}
	}
	return nil
}

// ProjectDeployEnabled reports whether deploys are turned on for a project.
func (c *Config) ProjectDeployEnabled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	return ok && pc.Deploy != nil && pc.Deploy.Enabled
}

// ProjectDeployManifestPath returns the effective in-repo manifest path.
func (c *Config) ProjectDeployManifestPath(name string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if pc, ok := c.Projects[name]; ok && pc.Deploy != nil {
		return strings.TrimSpace(pc.Deploy.ManifestPath)
	}
	return ""
}

// ProjectDeployEnv returns a copy of one environment's policy, secrets included.
// Callers must not log or render the Env map.
func (c *Config) ProjectDeployEnv(name, env string) (*DeployEnvConfig, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Deploy == nil {
		return nil, false
	}
	e, ok := pc.Deploy.Environments[env]
	if !ok || e == nil {
		return nil, false
	}
	return cloneDeployEnv(e), true
}

// ProjectDeployEnvNames returns configured environment names in stable order.
func (c *Config) ProjectDeployEnvNames(name string) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Deploy == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(pc.Deploy.Environments))
}

// SetProjectDeployEnabled turns deploys on or off for a project.
func (c *Config) SetProjectDeployEnabled(name string, enabled bool) error {
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		d.Enabled = enabled
		return nil
	})
}

// SetProjectDeployManifestPath overrides (or clears) the in-repo manifest path.
func (c *Config) SetProjectDeployManifestPath(name, path string) error {
	path = strings.TrimSpace(path)
	if path != "" {
		if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			return fmt.Errorf("manifest path must be relative and stay inside the repo")
		}
	}
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		d.ManifestPath = path
		return nil
	})
}

// DeployEnvPolicy is the editable, non-secret part of an environment.
type DeployEnvPolicy struct {
	RequireCapability string
	AllowedRefs       []string
	StepTimeoutMaxMs  *int
}

// SetProjectDeployEnvPolicy creates or updates one environment's policy. Secrets
// are untouched: they have their own write path so a policy edit can never
// clear a credential by omission.
func (c *Config) SetProjectDeployEnvPolicy(name, env string, pol DeployEnvPolicy) error {
	env = strings.TrimSpace(env)
	if err := ValidateDeployEnvName(env); err != nil {
		return err
	}
	pol.RequireCapability = strings.TrimSpace(pol.RequireCapability)
	if pol.RequireCapability != "" && !slices.Contains(ValidDeployCapabilities, pol.RequireCapability) {
		return fmt.Errorf("unknown capability %q (want one of %s)", pol.RequireCapability, strings.Join(ValidDeployCapabilities, ", "))
	}
	if pol.StepTimeoutMaxMs != nil {
		if *pol.StepTimeoutMaxMs <= 0 {
			return fmt.Errorf("step timeout ceiling must be positive")
		}
		if *pol.StepTimeoutMaxMs > DefaultDeployStepTimeoutMaxMs {
			return fmt.Errorf("step timeout ceiling may not exceed %dms", DefaultDeployStepTimeoutMaxMs)
		}
	}
	refs := cleanIDList(pol.AllowedRefs)
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		if d.Environments == nil {
			d.Environments = map[string]*DeployEnvConfig{}
		}
		e := d.Environments[env]
		if e == nil {
			e = &DeployEnvConfig{}
			d.Environments[env] = e
		}
		e.RequireCapability = pol.RequireCapability
		e.AllowedRefs = refs
		e.StepTimeoutMaxMs = cloneIntPtr(pol.StepTimeoutMaxMs)
		return nil
	})
}

// RemoveProjectDeployEnv deletes an environment and every credential in it.
func (c *Config) RemoveProjectDeployEnv(name, env string) error {
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		delete(d.Environments, strings.TrimSpace(env))
		return nil
	})
}

// SetProjectDeployEnvVar upserts one variable.
//
// An empty value keeps the stored one (the "leave blank to keep" contract the
// Linear API key already uses), so re-marking a key as secret does not require
// retyping the credential. secret is applied regardless.
func (c *Config) SetProjectDeployEnvVar(name, env, key, value string, secret bool) error {
	env = strings.TrimSpace(env)
	if err := ValidateDeployEnvName(env); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := ValidateDeployEnvKey(key); err != nil {
		return err
	}
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		if d.Environments == nil {
			d.Environments = map[string]*DeployEnvConfig{}
		}
		e := d.Environments[env]
		if e == nil {
			e = &DeployEnvConfig{}
			d.Environments[env] = e
		}
		if e.Env == nil {
			e.Env = map[string]string{}
		}
		prev, existed := e.Env[key]
		if value == "" {
			if !existed {
				return fmt.Errorf("a value is required for a new variable")
			}
			value = prev
		}
		e.Env[key] = value
		e.SecretKeys = slices.DeleteFunc(e.SecretKeys, func(k string) bool { return k == key })
		if secret {
			e.SecretKeys = append(e.SecretKeys, key)
			slices.Sort(e.SecretKeys)
		}
		return nil
	})
}

// RemoveProjectDeployEnvVar deletes one variable.
func (c *Config) RemoveProjectDeployEnvVar(name, env, key string) error {
	env = strings.TrimSpace(env)
	key = strings.TrimSpace(key)
	return c.mutateProjectDeploy(name, func(d *ProjectDeployConfig) error {
		e := d.Environments[env]
		if e == nil {
			return nil
		}
		delete(e.Env, key)
		e.SecretKeys = slices.DeleteFunc(e.SecretKeys, func(k string) bool { return k == key })
		if len(e.Env) == 0 {
			e.Env = nil
		}
		if len(e.SecretKeys) == 0 {
			e.SecretKeys = nil
		}
		return nil
	})
}

// mutateProjectDeploy applies fn to a project's deploy config and persists.
// Follows the package rule: validate before locking, mutate under the write
// lock, persist inside it, and nil an empty sub-config so config.json stays clean.
func (c *Config) mutateProjectDeploy(name string, fn func(*ProjectDeployConfig) error) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if c == nil {
		return fmt.Errorf("nil config")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	d := cloneProjectDeploy(pc.Deploy)
	if d == nil {
		d = &ProjectDeployConfig{}
	}
	if err := fn(d); err != nil {
		return err
	}
	pc.Deploy = normalizeProjectDeploy(d)
	c.Projects[name] = pc
	return c.saveLocked()
}

// DeployEnvKeyItem is one variable row for the settings UI: the name and whether
// it is redacted. Never the value.
type DeployEnvKeyItem struct {
	Key    string
	Secret bool
}

// DeployEnvItem is one environment row for the settings UI.
type DeployEnvItem struct {
	Name              string
	RequireCapability string
	AllowedRefsText   string
	StepTimeoutMaxMs  int // effective
	Keys              []DeployEnvKeyItem
	SecretCount       int
}

// deployItems builds the UI projection for one project. Values are deliberately
// absent: the template can render what exists without being able to leak it.
func deployItems(d *ProjectDeployConfig) (enabled bool, manifestPath string, envs []DeployEnvItem) {
	if d == nil {
		return false, "", nil
	}
	for _, name := range slices.Sorted(maps.Keys(d.Environments)) {
		e := d.Environments[name]
		if e == nil {
			continue
		}
		item := DeployEnvItem{
			Name:              name,
			RequireCapability: e.RequireCapability,
			AllowedRefsText:   strings.Join(e.AllowedRefs, "\n"),
			StepTimeoutMaxMs:  e.StepTimeoutMaxMsValue(),
		}
		for _, k := range slices.Sorted(maps.Keys(e.Env)) {
			secret := e.IsSecretKey(k)
			if secret {
				item.SecretCount++
			}
			item.Keys = append(item.Keys, DeployEnvKeyItem{Key: k, Secret: secret})
		}
		envs = append(envs, item)
	}
	return d.Enabled, d.ManifestPath, envs
}

// MaxConcurrentDeploysValue returns the host-wide cap (0 or unset → default).
func (c *Config) MaxConcurrentDeploysValue() int {
	if c == nil {
		return DefaultMaxConcurrentDeploys
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxConcurrentDeploys == nil || *c.MaxConcurrentDeploys <= 0 {
		return DefaultMaxConcurrentDeploys
	}
	return *c.MaxConcurrentDeploys
}

// DeployRunRetentionValue returns how many terminal runs to keep per lane.
func (c *Config) DeployRunRetentionValue() int {
	if c == nil {
		return DefaultDeployRunRetention
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.DeployRunRetention == nil || *c.DeployRunRetention <= 0 {
		return DefaultDeployRunRetention
	}
	return *c.DeployRunRetention
}
