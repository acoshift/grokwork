// Package deploy runs manual, per-service, per-environment deploy pipelines.
//
// The pipeline itself is declared in the repository at .grokwork/deploy.yaml and
// is read from the deployed commit, so it is versioned with the code it deploys
// and travels through the same review as any other change. Policy and
// credentials live in per-project config.json instead, because they must not be
// committed and differ per host.
//
// A manifest is therefore untrusted input: anyone who can push a branch can
// author the commands. Parsing is strict and every limit here is a blast-radius
// bound, not a style preference. The control that actually stops a hostile
// manifest reaching production credentials is the per-environment ref allowlist
// in config, not this file.
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultManifestPath is the in-repo location of the pipeline definition.
const DefaultManifestPath = ".grokwork/deploy.yaml"

// Limits bounding a manifest. These cap what a pushed commit can ask the host to
// do; they are deliberately generous for real pipelines and tight enough that a
// hostile manifest cannot turn one click into unbounded work.
const (
	MaxManifestBytes    = 64 << 10
	MaxEnvironments     = 16
	MaxServices         = 20
	MaxStepsPerPipeline = 30
	MaxCommandBytes     = 4000

	// DefaultStepTimeout applies to a step that declares none.
	DefaultStepTimeout = 15 * time.Minute
	// MaxStepTimeout is the ceiling a manifest may request. A project may lower
	// it further per environment via config (stepTimeoutMaxMs).
	MaxStepTimeout = time.Hour
)

// EnvNameRule is the environment-name rule shared with internal/config.
const EnvNameRule = `^[a-z][a-z0-9-]{0,31}$`

// ErrNoManifest reports that the commit has no manifest at the expected path.
// Callers render a "not configured" state rather than an error for this.
var ErrNoManifest = errors.New("deploy: no manifest in commit")

var (
	// nameRe matches a service or step name.
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// envNameRe matches an environment name. Stricter than nameRe: environment
	// names become path segments and env var values, and "." would be confusing.
	// EnvNameRule is duplicated as config.DeployEnvNameRule — this package will
	// need internal/config for per-environment credentials, so config cannot
	// import it back. TestEnvNameRuleMatchesConfig pins the two together.
	envNameRe = regexp.MustCompile(EnvNameRule)
)

// Runner runs a command in dir and returns stdout. Tests inject fakes.
// Structurally identical to ghpr.Runner; declared here so this package does not
// depend on the GitHub layer for what is plain git plumbing.
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// Step is one shell command in a pipeline.
type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// Timeout is a Go duration string ("90s", "20m"). Empty → DefaultStepTimeout.
	Timeout string `yaml:"timeout,omitempty"`
	// Envs, when set, restricts this step to the listed environments. It is the
	// economical answer to "same pipeline, plus one extra step in prod"; a
	// genuinely different pipeline belongs in Service.Pipelines instead.
	Envs []string `yaml:"envs,omitempty"`
}

// Service is one deployable unit of a (possibly mono-) repo.
type Service struct {
	// Dir is the working directory for every step, relative to the repo root.
	// Empty means the repo root.
	Dir string `yaml:"dir,omitempty"`
	// Envs, when set, narrows which environments this service deploys to.
	Envs []string `yaml:"envs,omitempty"`
	// Steps is the default pipeline, used by any environment with no Pipelines entry.
	Steps []Step `yaml:"steps,omitempty"`
	// Pipelines holds per-environment pipelines that fully replace Steps.
	Pipelines map[string][]Step `yaml:"pipelines,omitempty"`
}

// Manifest is a parsed, validated .grokwork/deploy.yaml.
type Manifest struct {
	Version      int      `yaml:"version"`
	Environments []string `yaml:"environments"`
	// Anchors is a reserved parking spot for YAML anchor definitions. Strict
	// decoding rejects every unknown top-level key, so without a declared home
	// an anchor block cannot be written at all. Held as a raw node: nothing in
	// it is expanded, validated, or executed.
	Anchors  yaml.Node          `yaml:"x-anchors,omitempty"`
	Services map[string]Service `yaml:"services"`
}

// ResolvedStep is one step of a concrete (service, environment) pipeline, with
// the timeout already parsed. Resolved lists are frozen onto a run record so a
// redeploy replays exactly what ran, even if the branch has moved on.
type ResolvedStep struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs"`
}

// Parse decodes and validates manifest bytes.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("deploy: manifest is %d bytes, over the %d byte limit", len(data), MaxManifestBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("deploy: manifest is empty")
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Strict: a typo silently dropping a deploy step is worse than a hard failure.
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Version != 1 {
		return fmt.Errorf("deploy: version must be 1, got %d", m.Version)
	}
	if len(m.Environments) == 0 {
		return errors.New("deploy: environments is required and must list at least one name")
	}
	if len(m.Environments) > MaxEnvironments {
		return fmt.Errorf("deploy: %d environments, over the limit of %d", len(m.Environments), MaxEnvironments)
	}
	seenEnv := make(map[string]bool, len(m.Environments))
	for i, env := range m.Environments {
		if !envNameRe.MatchString(env) {
			return fmt.Errorf("deploy: environments[%d]: invalid name %q (want %s)", i, env, envNameRe)
		}
		if seenEnv[env] {
			return fmt.Errorf("deploy: environments[%d]: duplicate name %q", i, env)
		}
		seenEnv[env] = true
	}

	if len(m.Services) == 0 {
		return errors.New("deploy: services is required and must define at least one service")
	}
	if len(m.Services) > MaxServices {
		return fmt.Errorf("deploy: %d services, over the limit of %d", len(m.Services), MaxServices)
	}
	for _, name := range slices.Sorted(maps.Keys(m.Services)) {
		if err := m.validateService(name, m.Services[name]); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manifest) validateService(name string, svc Service) error {
	where := "services." + name
	if !nameRe.MatchString(name) {
		return fmt.Errorf("deploy: %s: invalid service name (want %s)", where, nameRe)
	}
	if err := validateDir(svc.Dir); err != nil {
		return fmt.Errorf("deploy: %s.dir: %w", where, err)
	}
	for i, env := range svc.Envs {
		if !m.hasEnvironment(env) {
			return fmt.Errorf("deploy: %s.envs[%d]: unknown environment %q", where, i, env)
		}
	}
	if len(svc.Steps) == 0 && len(svc.Pipelines) == 0 {
		return fmt.Errorf("deploy: %s: needs steps or at least one pipelines entry", where)
	}
	if len(svc.Steps) > 0 {
		if err := m.validateSteps(where+".steps", svc.Steps); err != nil {
			return err
		}
	}
	for _, env := range slices.Sorted(maps.Keys(svc.Pipelines)) {
		if !m.hasEnvironment(env) {
			return fmt.Errorf("deploy: %s.pipelines: unknown environment %q", where, env)
		}
		// A pipeline declared for an environment the service excludes is a
		// contradiction, and silently honouring one of the two would surprise.
		if len(svc.Envs) > 0 && !slices.Contains(svc.Envs, env) {
			return fmt.Errorf("deploy: %s.pipelines.%s: environment is excluded by %s.envs", where, env, where)
		}
		steps := svc.Pipelines[env]
		if len(steps) == 0 {
			return fmt.Errorf("deploy: %s.pipelines.%s: needs at least one step", where, env)
		}
		if err := m.validateSteps(fmt.Sprintf("%s.pipelines.%s", where, env), steps); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manifest) validateSteps(where string, steps []Step) error {
	if len(steps) > MaxStepsPerPipeline {
		return fmt.Errorf("deploy: %s: %d steps, over the limit of %d", where, len(steps), MaxStepsPerPipeline)
	}
	seen := make(map[string]bool, len(steps))
	for i, st := range steps {
		at := fmt.Sprintf("%s[%d]", where, i)
		if !nameRe.MatchString(st.Name) {
			return fmt.Errorf("deploy: %s.name: invalid step name %q (want %s)", at, st.Name, nameRe)
		}
		if seen[st.Name] {
			return fmt.Errorf("deploy: %s.name: duplicate step name %q", at, st.Name)
		}
		seen[st.Name] = true
		if strings.TrimSpace(st.Run) == "" {
			return fmt.Errorf("deploy: %s.run: command is required", at)
		}
		if len(st.Run) > MaxCommandBytes {
			return fmt.Errorf("deploy: %s.run: command is %d bytes, over the limit of %d", at, len(st.Run), MaxCommandBytes)
		}
		if _, err := stepTimeout(st.Timeout); err != nil {
			return fmt.Errorf("deploy: %s.timeout: %w", at, err)
		}
		for j, env := range st.Envs {
			if !m.hasEnvironment(env) {
				return fmt.Errorf("deploy: %s.envs[%d]: unknown environment %q", at, j, env)
			}
		}
	}
	return nil
}

// stepTimeout parses a step's declared timeout, applying the default and ceiling.
func stepTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultStepTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (want e.g. 90s, 20m)", raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	if d > MaxStepTimeout {
		return 0, fmt.Errorf("%s is over the ceiling of %s", d, MaxStepTimeout)
	}
	return d, nil
}

// validateDir rejects anything that could place a step's cwd outside the checkout.
func validateDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." {
		return nil
	}
	if strings.HasPrefix(dir, "/") {
		return errors.New("must be relative to the repo root")
	}
	if strings.ContainsRune(dir, '\\') {
		return errors.New("must use / as the separator")
	}
	clean := path.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("must stay inside the repo")
	}
	return nil
}

func (m *Manifest) hasEnvironment(env string) bool {
	return slices.Contains(m.Environments, env)
}

// ServiceNames returns service names in stable order.
func (m *Manifest) ServiceNames() []string {
	return slices.Sorted(maps.Keys(m.Services))
}

// Deployable reports whether service may deploy to env at all, ignoring config
// policy (which the caller checks separately).
func (m *Manifest) Deployable(service, env string) bool {
	steps, err := m.Resolve(service, env)
	return err == nil && len(steps) > 0
}

// Resolve returns the frozen step list for one (service, environment).
//
// Selection order: an explicit pipelines entry fully replaces the default steps;
// then any step restricted to other environments is dropped.
func (m *Manifest) Resolve(service, env string) ([]ResolvedStep, error) {
	svc, ok := m.Services[service]
	if !ok {
		return nil, fmt.Errorf("deploy: unknown service %q", service)
	}
	if !m.hasEnvironment(env) {
		return nil, fmt.Errorf("deploy: unknown environment %q", env)
	}
	if len(svc.Envs) > 0 && !slices.Contains(svc.Envs, env) {
		return nil, fmt.Errorf("deploy: service %q does not deploy to %q", service, env)
	}
	steps, ok := svc.Pipelines[env]
	if !ok {
		steps = svc.Steps
	}
	out := make([]ResolvedStep, 0, len(steps))
	for _, st := range steps {
		if len(st.Envs) > 0 && !slices.Contains(st.Envs, env) {
			continue
		}
		d, err := stepTimeout(st.Timeout)
		if err != nil {
			// Unreachable after validate(); treated as a hard error rather than
			// silently substituting a default for a value someone chose.
			return nil, fmt.Errorf("deploy: service %q step %q: %w", service, st.Name, err)
		}
		out = append(out, ResolvedStep{
			Name:      st.Name,
			Command:   st.Run,
			TimeoutMs: int(d / time.Millisecond),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("deploy: service %q has no steps for %q", service, env)
	}
	return out, nil
}

// Dir returns the step working directory for a service, relative to the repo root.
func (m *Manifest) Dir(service string) string {
	svc, ok := m.Services[service]
	if !ok {
		return "."
	}
	dir := strings.TrimSpace(svc.Dir)
	if dir == "" {
		return "."
	}
	return path.Clean(dir)
}

// LoadAt reads and parses the manifest as it exists at rev.
//
// Reading from the revision rather than the working tree is what makes a deploy
// reproducible: the steps that run are the ones committed alongside the code
// being deployed, not whatever a parallel agent has left in the shared checkout.
//
// The size is probed with ls-tree before any read, because a Runner buffers all
// of stdout in memory — checking the cap after reading would not be a cap.
func LoadAt(ctx context.Context, run Runner, repoPath, rev, manifestPath string) (*Manifest, error) {
	if run == nil {
		run = execRunner
	}
	manifestPath = strings.TrimSpace(manifestPath)
	if manifestPath == "" {
		manifestPath = DefaultManifestPath
	}
	if err := validateDir(manifestPath); err != nil {
		return nil, fmt.Errorf("deploy: manifest path %q: %w", manifestPath, err)
	}
	blob, size, err := lsTreeBlob(ctx, run, repoPath, rev, manifestPath)
	if err != nil {
		return nil, err
	}
	if size > MaxManifestBytes {
		return nil, fmt.Errorf("deploy: %s is %d bytes at %s, over the %d byte limit",
			manifestPath, size, shortRev(rev), MaxManifestBytes)
	}
	out, err := run(ctx, repoPath, "git", "cat-file", "blob", blob)
	if err != nil {
		return nil, fmt.Errorf("deploy: read %s at %s: %w", manifestPath, shortRev(rev), err)
	}
	m, err := Parse(out)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// lsTreeBlob returns the blob id and size of one path at rev.
//
// ls-tree exits 0 with empty output for a path that does not exist, so absence
// is a value rather than an error string to match on.
func lsTreeBlob(ctx context.Context, run Runner, repoPath, rev, filePath string) (blob string, size int64, err error) {
	out, err := run(ctx, repoPath, "git", "ls-tree", "-l", "--full-tree", rev, "--", filePath)
	if err != nil {
		return "", 0, fmt.Errorf("deploy: locate %s at %s: %w", filePath, shortRev(rev), err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", 0, ErrNoManifest
	}
	// "<mode> <type> <object>  <size>\t<path>"
	meta, _, ok := strings.Cut(line, "\t")
	if !ok {
		return "", 0, fmt.Errorf("deploy: unexpected ls-tree output for %s", filePath)
	}
	fields := strings.Fields(meta)
	if len(fields) != 4 {
		return "", 0, fmt.Errorf("deploy: unexpected ls-tree output for %s", filePath)
	}
	if fields[1] != "blob" {
		// A directory or submodule at the manifest path is "not configured",
		// not a corrupt repo.
		return "", 0, ErrNoManifest
	}
	size, err = strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("deploy: unreadable size for %s: %w", filePath, err)
	}
	return fields[2], size, nil
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
