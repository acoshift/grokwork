package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ProjectStorageConfig is per-project file storage on Google Cloud Storage.
//
// The bucket is provisioned out of band; grokwork only reads and writes objects
// under an optional prefix. Auth is the host's gcloud ADC — no credentials here.
type ProjectStorageConfig struct {
	GCSBucket string `json:"gcsBucket"`
	// Prefix is an optional object-name prefix (no leading/trailing slash).
	Prefix string `json:"prefix,omitempty"`
}

// gcsBucketRe is a conservative subset of GCS bucket-name rules. The name is
// spliced into a gs:// URL handed to a CLI that expands wildcards, so anything
// outside this charset is refused even if GCS itself would accept it.
var gcsBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

func cloneProjectStorage(s *ProjectStorageConfig) *ProjectStorageConfig {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// normalizeProjectStorage trims and validates. Empty bucket → nil so config.json
// never grows an empty object. An invalid bucket/prefix is a hard error (like
// normalizeSLA): a typo silently disabling storage looks identical to storage
// being off.
func normalizeProjectStorage(s *ProjectStorageConfig) (*ProjectStorageConfig, error) {
	if s == nil {
		return nil, nil
	}
	s.GCSBucket = strings.TrimSpace(s.GCSBucket)
	s.Prefix = strings.TrimSpace(s.Prefix)
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
	return s, nil
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

// ProjectStorage returns a copy of a project's storage config, or nil when
// unlinked.
func (c *Config) ProjectStorage(name string) *ProjectStorageConfig {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	pc, ok := c.Projects[name]
	if !ok || pc.Storage == nil {
		return nil
	}
	return cloneProjectStorage(pc.Storage)
}

// SetProjectStorageGCS links (or unlinks) a project's GCS bucket. An empty
// bucket clears the link and nils the sub-config.
func (c *Config) SetProjectStorageGCS(project, bucket, prefix string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	bucket = strings.TrimSpace(bucket)
	prefix = strings.TrimSpace(prefix)
	// Validate before locking so a bad form never holds the write lock.
	var next *ProjectStorageConfig
	if bucket != "" {
		if err := validateGCSBucket(bucket); err != nil {
			return err
		}
		cleanPrefix, err := validateStoragePrefix(prefix)
		if err != nil {
			return err
		}
		next = &ProjectStorageConfig{GCSBucket: bucket, Prefix: cleanPrefix}
	}
	return c.mutateProjectStorage(project, func(_ *ProjectStorageConfig) (*ProjectStorageConfig, error) {
		return next, nil
	})
}

// mutateProjectStorage applies fn to a project's storage config and persists.
// fn returns the replacement (may be nil to clear). Validate before locking;
// mutate and persist under the write lock; nil an empty sub-config.
func (c *Config) mutateProjectStorage(name string, fn func(*ProjectStorageConfig) (*ProjectStorageConfig, error)) error {
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
	cur := cloneProjectStorage(pc.Storage)
	next, err := fn(cur)
	if err != nil {
		return err
	}
	next, err = normalizeProjectStorage(next)
	if err != nil {
		return err
	}
	pc.Storage = next
	c.Projects[name] = pc
	return c.saveLocked()
}
