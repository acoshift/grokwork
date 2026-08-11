package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// StorageConfig is GCS file storage: the global host default and/or a
// per-project override. Empty bucket after normalize yields nil (except
// disabled project blocks, which keep Disabled only).
type StorageConfig struct {
	GCSBucket string `json:"gcsBucket,omitempty"`
	// Prefix is an optional object-name prefix (no leading/trailing slash).
	Prefix string `json:"prefix,omitempty"`
	// CredentialsFile is an optional absolute path to a service-account JSON
	// key on the host, passed to gcloud per invocation
	// (CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE) so it never rewrites the host's
	// global gcloud auth. The path — never the key material — lives in config.
	CredentialsFile string `json:"credentialsFile,omitempty"`
	// Disabled is project-only: storage is off for this project even when a
	// global default exists. Forbidden on the global block (load + setter error).
	// When true, other fields are ignored and stripped on normalize.
	Disabled bool `json:"disabled,omitempty"`
}

// Storage source labels for Snapshot / UI.
const (
	StorageSourceNone     = "none"
	StorageSourceGlobal   = "global"
	StorageSourceOverride = "override"
	StorageSourceDisabled = "disabled"
)

// gcsBucketRe is a conservative subset of GCS bucket-name rules. The name is
// spliced into a gs:// URL handed to a CLI that expands wildcards, so anything
// outside this charset is refused even if GCS itself would accept it.
var gcsBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// storageSegmentRe is the post-condition for a single project path segment
// under a shared global prefix.
var storageSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

func cloneStorage(s *StorageConfig) *StorageConfig {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// normalizeStorage trims and validates.
// projectContext: when true, Disabled is allowed and yields {Disabled:true}.
// when false (global), Disabled is a hard error if set.
func normalizeStorage(s *StorageConfig, projectContext bool) (*StorageConfig, error) {
	if s == nil {
		return nil, nil
	}
	s.GCSBucket = strings.TrimSpace(s.GCSBucket)
	s.Prefix = strings.TrimSpace(s.Prefix)
	s.CredentialsFile = strings.TrimSpace(s.CredentialsFile)
	if s.Disabled {
		if !projectContext {
			return nil, fmt.Errorf("disabled is only valid on projects.*.storage")
		}
		return &StorageConfig{Disabled: true}, nil
	}
	if s.GCSBucket == "" {
		return nil, nil
	}
	if err := validateGCSBucket(s.GCSBucket); err != nil {
		return nil, err
	}
	prefix, err := validateStoragePrefix(s.Prefix)
	if err != nil {
		return nil, err
	}
	s.Prefix = prefix
	if err := validateStorageCredentialsFile(s.CredentialsFile); err != nil {
		return nil, err
	}
	return s, nil
}

// validateStorageCredentialsFile requires an absolute path (matching the
// project-path rule): a relative path silently resolves against whatever cwd
// the bot happened to start in. Existence is deliberately not checked — the
// key may be provisioned after the config, and gcloud reports a missing file
// legibly at use.
func validateStorageCredentialsFile(p string) error {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("storage credentialsFile must be an absolute path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return fmt.Errorf("storage credentialsFile must not contain control characters")
		}
	}
	return nil
}

func validateGCSBucket(bucket string) error {
	if len(bucket) < 3 || len(bucket) > 222 {
		return fmt.Errorf("gcs bucket name must be 3–222 characters")
	}
	if !gcsBucketRe.MatchString(bucket) {
		return fmt.Errorf("invalid gcs bucket name %q (lowercase letters, digits, -, _, .; must start and end with a letter or digit)", bucket)
	}
	return nil
}

// validateStoragePrefix rejects path traversal, wildcards, and control chars.
// A trailing slash is stripped; a leading slash is refused.
func validateStoragePrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	if strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("storage prefix must not start with /")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	for part := range strings.SplitSeq(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("storage prefix has an empty or '.'/'..' segment")
		}
	}
	if strings.ContainsAny(prefix, "*?[]") {
		return "", fmt.Errorf("storage prefix must not contain wildcard characters")
	}
	for _, r := range prefix {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return "", fmt.Errorf("storage prefix must not contain control characters")
		}
	}
	return prefix, nil
}

// storageProjectSegment returns a single safe object-name segment for a project.
// Post-conditions: non-empty; matches storageSegmentRe; length ≤ 63; never "".
func storageProjectSegment(project string) string {
	raw := strings.TrimSpace(project)
	if raw != "" && len(raw) <= 63 && storageSegmentRe.MatchString(raw) {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw))
	prevUnderscore := false
	for _, r := range raw {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			prevUnderscore = r == '_'
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	s := strings.Trim(b.String(), "_.")
	if s != "" && len(s) <= 63 && storageSegmentRe.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("p_%x", sum[:8]) // 16 hex chars
}

// JoinStoragePrefix joins optional base prefix with storageProjectSegment(project).
// Re-validates the full string with validateStoragePrefix.
func JoinStoragePrefix(base, project string) (string, error) {
	seg := storageProjectSegment(project)
	base = strings.TrimSpace(base)
	joined := seg
	if base != "" {
		joined = base + "/" + seg
	}
	return validateStoragePrefix(joined)
}

// GlobalStorage returns a copy of the host default, or nil.
func (c *Config) GlobalStorage() *StorageConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStorage(c.Storage)
}

// ProjectStorage returns the project's raw stored block (override or disabled),
// or nil when the project inherits. Does NOT apply global fallback.
func (c *Config) ProjectStorage(name string) *StorageConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Storage == nil {
		return nil
	}
	return cloneStorage(pc.Storage)
}

// EffectiveStorage is the single resolution entry for Files I/O.
// On join/validate failure returns nil (fail closed); never returns global
// with an unjoined prefix.
func (c *Config) EffectiveStorage(name string) *StorageConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.effectiveStorageLocked(name)
}

func (c *Config) effectiveStorageLocked(name string) *StorageConfig {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	pc, ok := c.Projects[name]
	if !ok {
		return nil
	}
	raw := pc.Storage
	if raw != nil && raw.Disabled {
		return nil
	}
	if raw != nil && strings.TrimSpace(raw.GCSBucket) != "" {
		return cloneStorage(raw)
	}
	if c.Storage == nil || strings.TrimSpace(c.Storage.GCSBucket) == "" {
		return nil
	}
	prefix, err := JoinStoragePrefix(c.Storage.Prefix, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] effective storage for project %q: %v\n", name, err)
		return nil
	}
	out := cloneStorage(c.Storage)
	out.Prefix = prefix
	out.Disabled = false
	return out
}

func (c *Config) storageSourceLocked(name string) string {
	name = strings.TrimSpace(name)
	pc, ok := c.Projects[name]
	if !ok {
		return StorageSourceNone
	}
	raw := pc.Storage
	if raw != nil && raw.Disabled {
		return StorageSourceDisabled
	}
	if raw != nil && strings.TrimSpace(raw.GCSBucket) != "" {
		return StorageSourceOverride
	}
	if c.Storage != nil && strings.TrimSpace(c.Storage.GCSBucket) != "" {
		return StorageSourceGlobal
	}
	return StorageSourceNone
}

// SetGlobalStorageGCS sets or clears the host default.
// Empty bucket clears (nils c.Storage). Disabled is never accepted.
func (c *Config) SetGlobalStorageGCS(bucket, prefix, credentialsFile string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	bucket = strings.TrimSpace(bucket)
	prefix = strings.TrimSpace(prefix)
	credentialsFile = strings.TrimSpace(credentialsFile)
	var next *StorageConfig
	if bucket != "" {
		if err := validateGCSBucket(bucket); err != nil {
			return err
		}
		cleanPrefix, err := validateStoragePrefix(prefix)
		if err != nil {
			return err
		}
		if err := validateStorageCredentialsFile(credentialsFile); err != nil {
			return err
		}
		next = &StorageConfig{GCSBucket: bucket, Prefix: cleanPrefix, CredentialsFile: credentialsFile}
	}
	next, err := normalizeStorage(next, false)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Storage = next
	return c.saveLocked()
}

// SetProjectStorageGCS sets a full override. Requires a non-empty bucket.
// Empty bucket is an error — use ClearProjectStorage or SetProjectStorageDisabled.
func (c *Config) SetProjectStorageGCS(project, bucket, prefix, credentialsFile string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	bucket = strings.TrimSpace(bucket)
	prefix = strings.TrimSpace(prefix)
	credentialsFile = strings.TrimSpace(credentialsFile)
	if bucket == "" {
		return fmt.Errorf("gcsBucket is required; use ClearProjectStorage to re-inherit/unlink, or SetProjectStorageDisabled to turn Files off")
	}
	if err := validateGCSBucket(bucket); err != nil {
		return err
	}
	cleanPrefix, err := validateStoragePrefix(prefix)
	if err != nil {
		return err
	}
	if err := validateStorageCredentialsFile(credentialsFile); err != nil {
		return err
	}
	next := &StorageConfig{GCSBucket: bucket, Prefix: cleanPrefix, CredentialsFile: credentialsFile}
	return c.mutateProjectStorage(project, func(_ *StorageConfig) (*StorageConfig, error) {
		return next, nil
	})
}

// ClearProjectStorage nils projects[name].Storage.
// With global set the project re-inherits; without global it has no storage.
func (c *Config) ClearProjectStorage(project string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	return c.mutateProjectStorage(project, func(_ *StorageConfig) (*StorageConfig, error) {
		return nil, nil
	})
}

// SetProjectStorageDisabled stores {disabled: true}, turning Files off for
// this project regardless of global.
func (c *Config) SetProjectStorageDisabled(project string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	return c.mutateProjectStorage(project, func(_ *StorageConfig) (*StorageConfig, error) {
		return &StorageConfig{Disabled: true}, nil
	})
}

// mutateProjectStorage applies fn to a project's storage config and persists.
// fn returns the replacement (may be nil to clear). Validate before locking;
// mutate and persist under the write lock; nil an empty sub-config.
func (c *Config) mutateProjectStorage(name string, fn func(*StorageConfig) (*StorageConfig, error)) error {
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
	cur := cloneStorage(pc.Storage)
	next, err := fn(cur)
	if err != nil {
		return err
	}
	next, err = normalizeStorage(next, true)
	if err != nil {
		return err
	}
	pc.Storage = next
	c.Projects[name] = pc
	return c.saveLocked()
}

// CountInheritingStorageProjects returns how many projects would inherit the
// global default (raw nil, not disabled). Used by the global Save confirm.
func (c *Config) CountInheritingStorageProjects() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, pc := range c.Projects {
		if pc.Storage == nil {
			n++
		}
	}
	return n
}
