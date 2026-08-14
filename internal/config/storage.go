package config

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Backend identifiers persisted in config and projected to the UI.
const (
	StorageBackendGCS    = "gcs"
	StorageBackendGDrive = "gdrive"
)

// StorageConfig is file storage: global host default and/or per-project override.
type StorageConfig struct {
	// Backend is "gcs" or "gdrive". Empty is normalized from identity fields
	// (see normalizeStorage). Persisted once set so Snapshot/forms stay stable.
	Backend string `json:"backend,omitempty"`

	GCSBucket string `json:"gcsBucket,omitempty"`
	// Prefix is an optional object-name prefix (GCS only; no leading/trailing slash).
	Prefix string `json:"prefix,omitempty"`

	// DriveFolderID is the Drive folder id for this block (not a path).
	// Required when Backend is gdrive.
	DriveFolderID string `json:"driveFolderId,omitempty"`

	// CredentialsFile is an absolute path to a service-account JSON key.
	// Global Drive requires it. A project override may leave it empty to use
	// the global path (EffectiveStorage fills that in). Empty on GCS with no
	// global key means host gcloud ADC.
	CredentialsFile string `json:"credentialsFile,omitempty"`

	// Disabled is project-only (unchanged).
	Disabled bool `json:"disabled,omitempty"`

	// IsolationSegment is set only by EffectiveStorage when a Drive root is
	// shared (inherit, or an override that reuses the global folder).
	// Never persisted (json:"-"). The Drive adapter find-or-creates a child
	// folder with this name under DriveFolderID. Empty on a dedicated
	// override folder / GCS.
	IsolationSegment string `json:"-"`
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

// driveFolderIDRe is a conservative charset for Google Drive folder ids.
var driveFolderIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,128}$`)

func cloneStorage(s *StorageConfig) *StorageConfig {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// storageHasIdentity reports whether s is a usable storage identity after normalize.
// Precondition: s is nil, disabled-only, or fully normalized (Backend always set when non-nil non-disabled).
// Empty Backend is fail-closed except the legacy GCS defense: treat as gcs only when
// bucket is set AND folder id is empty. Folder-only with empty backend returns false
// (caller should have normalized; do not silently treat as gdrive).
func storageHasIdentity(s *StorageConfig) bool {
	if s == nil || s.Disabled {
		return false
	}
	switch strings.TrimSpace(strings.ToLower(s.Backend)) {
	case StorageBackendGCS:
		return strings.TrimSpace(s.GCSBucket) != ""
	case StorageBackendGDrive:
		return strings.TrimSpace(s.DriveFolderID) != ""
	case "":
		// Defense only: pre-normalize or hand-built test fixtures.
		// Fail closed on folder-only (empty backend + driveFolderId).
		if strings.TrimSpace(s.DriveFolderID) != "" {
			return false
		}
		return strings.TrimSpace(s.GCSBucket) != ""
	default:
		return false
	}
}

// normalizeStorage trims and validates.
// projectContext: when true, Disabled is allowed and yields {Disabled:true}.
// when false (global), Disabled is a hard error if set.
func normalizeStorage(s *StorageConfig, projectContext bool) (*StorageConfig, error) {
	if s == nil {
		return nil, nil
	}
	s.Backend = strings.ToLower(strings.TrimSpace(s.Backend))
	s.GCSBucket = strings.TrimSpace(s.GCSBucket)
	s.Prefix = strings.TrimSpace(s.Prefix)
	s.DriveFolderID = strings.TrimSpace(s.DriveFolderID)
	s.CredentialsFile = strings.TrimSpace(s.CredentialsFile)
	s.IsolationSegment = "" // never load from disk

	if s.Disabled {
		if !projectContext {
			return nil, fmt.Errorf("disabled is only valid on projects.*.storage")
		}
		return &StorageConfig{Disabled: true}, nil
	}

	// Parse folder URL into id when it looks like a URL or contains /folders/.
	if s.DriveFolderID != "" {
		id, err := normalizeDriveFolderID(s.DriveFolderID)
		if err != nil {
			return nil, err
		}
		s.DriveFolderID = id
	}

	// Infer backend when empty.
	if s.Backend == "" {
		switch {
		case s.GCSBucket != "" && s.DriveFolderID == "":
			s.Backend = StorageBackendGCS
		case s.DriveFolderID != "" && s.GCSBucket == "":
			s.Backend = StorageBackendGDrive
		case s.GCSBucket == "" && s.DriveFolderID == "":
			return nil, nil // empty block
		default:
			return nil, fmt.Errorf("storage: set backend explicitly when both gcsBucket and driveFolderId are set")
		}
	}

	switch s.Backend {
	case StorageBackendGCS:
		if s.GCSBucket == "" {
			return nil, fmt.Errorf("gcsBucket is required when backend is gcs")
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
		// Strip foreign fields even if pasted.
		s.DriveFolderID = ""
		s.Backend = StorageBackendGCS
		s.IsolationSegment = ""
		return s, nil

	case StorageBackendGDrive:
		if s.DriveFolderID == "" {
			return nil, fmt.Errorf("driveFolderId is required when backend is gdrive")
		}
		// Id already normalized above; re-validate charset after strip.
		if !driveFolderIDRe.MatchString(s.DriveFolderID) {
			return nil, fmt.Errorf("invalid driveFolderId %q", s.DriveFolderID)
		}
		if s.CredentialsFile == "" && !projectContext {
			return nil, fmt.Errorf("credentialsFile is required when backend is gdrive")
		}
		if err := validateStorageCredentialsFile(s.CredentialsFile); err != nil {
			return nil, err
		}
		// Strip foreign fields.
		s.GCSBucket = ""
		s.Prefix = ""
		s.Backend = StorageBackendGDrive
		s.IsolationSegment = ""
		return s, nil

	default:
		return nil, fmt.Errorf("storage: unknown backend %q", s.Backend)
	}
}

// normalizeDriveFolderID accepts:
//   - bare id: 0ABcd… / 1AbC…
//   - https://drive.google.com/drive/folders/<id>[?…]
//   - https://drive.google.com/drive/u/0/folders/<id>
//   - folders/<id>
//
// Rejects empty, path traversal, spaces, control chars.
// Id charset: [A-Za-z0-9_-], length 10–128 (conservative).
func normalizeDriveFolderID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("driveFolderId is empty")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return "", fmt.Errorf("driveFolderId must not contain control characters")
		}
	}
	if strings.ContainsAny(raw, " \t\n\r") {
		return "", fmt.Errorf("driveFolderId must not contain spaces")
	}

	id := raw
	// URL form
	if strings.Contains(raw, "://") || strings.Contains(raw, "/folders/") || strings.HasPrefix(raw, "folders/") {
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			// Prefer path segment after /folders/
			if i := strings.Index(u.Path, "/folders/"); i >= 0 {
				rest := u.Path[i+len("/folders/"):]
				id = strings.SplitN(rest, "/", 2)[0]
			} else if strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "folders/") {
				rest := strings.TrimPrefix(strings.TrimPrefix(u.Path, "/"), "folders/")
				id = strings.SplitN(rest, "/", 2)[0]
			}
		} else if i := strings.Index(raw, "folders/"); i >= 0 {
			rest := raw[i+len("folders/"):]
			// strip query/fragment
			rest = strings.SplitN(rest, "?", 2)[0]
			rest = strings.SplitN(rest, "#", 2)[0]
			id = strings.SplitN(rest, "/", 2)[0]
		}
	}
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid driveFolderId")
	}
	if !driveFolderIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid driveFolderId %q (expected 10–128 chars of A-Za-z0-9_-)", id)
	}
	return id, nil
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
// with an unjoined prefix / missing isolation segment.
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
	if storageHasIdentity(raw) {
		// Override identity as-is, except a shared global root is isolated
		// the same way inherit is (project-named prefix / Drive child).
		// Empty credentials inherit the global key so a project can name a
		// bucket/folder without repeating the host SA path.
		out := cloneStorage(raw)
		if out.CredentialsFile == "" && c.Storage != nil {
			if creds := strings.TrimSpace(c.Storage.CredentialsFile); creds != "" {
				out.CredentialsFile = creds
			}
		}
		if sharesGlobalRoot(out, c.Storage) {
			if err := isolateOntoProject(out, c.Storage, name); err != nil {
				fmt.Fprintf(os.Stderr, "[warn] effective storage for project %q: %v\n", name, err)
				return nil
			}
		}
		return out
	}
	if !storageHasIdentity(c.Storage) {
		return nil
	}
	out := cloneStorage(c.Storage)
	if err := isolateOntoProject(out, c.Storage, name); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] effective storage for project %q: %v\n", name, err)
		return nil
	}
	out.Disabled = false
	return out
}

func storageBackendOf(s *StorageConfig) string {
	if s == nil {
		return ""
	}
	b := strings.TrimSpace(strings.ToLower(s.Backend))
	if b != "" {
		return b
	}
	if strings.TrimSpace(s.GCSBucket) != "" {
		return StorageBackendGCS
	}
	return ""
}

// sharesGlobalRoot reports whether an override points at the same Drive folder
// or the same GCS bucket with an empty / identical prefix — i.e. "using the
// default config" rather than a dedicated namespace.
func sharesGlobalRoot(override, global *StorageConfig) bool {
	if !storageHasIdentity(override) || !storageHasIdentity(global) {
		return false
	}
	if storageBackendOf(override) != storageBackendOf(global) {
		return false
	}
	switch storageBackendOf(override) {
	case StorageBackendGDrive:
		return override.DriveFolderID != "" && override.DriveFolderID == global.DriveFolderID
	case StorageBackendGCS, "":
		if override.GCSBucket == "" || override.GCSBucket != global.GCSBucket {
			return false
		}
		op := strings.TrimSpace(override.Prefix)
		gp := strings.TrimSpace(global.Prefix)
		return op == "" || op == gp
	default:
		return false
	}
}

// isolateOntoProject appends the project segment to a shared root.
// GCS uses global.Prefix as the base when the override left prefix empty so
// the result matches inherit (`{globalPrefix}/{project}`).
func isolateOntoProject(out, global *StorageConfig, project string) error {
	if out == nil {
		return fmt.Errorf("storage: nil block")
	}
	switch storageBackendOf(out) {
	case StorageBackendGCS, "":
		base := strings.TrimSpace(out.Prefix)
		if base == "" && global != nil {
			base = strings.TrimSpace(global.Prefix)
		}
		prefix, err := JoinStoragePrefix(base, project)
		if err != nil {
			return err
		}
		out.Prefix = prefix
		return nil
	case StorageBackendGDrive:
		out.IsolationSegment = storageProjectSegment(project)
		return nil
	default:
		return fmt.Errorf("storage: unknown backend %q", out.Backend)
	}
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
	if storageHasIdentity(raw) {
		return StorageSourceOverride
	}
	if storageHasIdentity(c.Storage) {
		return StorageSourceGlobal
	}
	return StorageSourceNone
}

// StorageInput is the structured form for save (global or project).
type StorageInput struct {
	Backend         string // "gcs" | "gdrive" | "" (infer)
	GCSBucket       string
	Prefix          string
	DriveFolderID   string
	CredentialsFile string
}

// SetGlobalStorage sets or clears the host default.
// After normalizeStorage(next, false):
//   - storageHasIdentity(next) → set c.Storage = next
//   - else → nil c.Storage (clear). This is the only global clear path.
//
// Disabled never accepted (normalize with projectContext=false).
func (c *Config) SetGlobalStorage(in StorageInput) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	next := &StorageConfig{
		Backend:         in.Backend,
		GCSBucket:       in.GCSBucket,
		Prefix:          in.Prefix,
		DriveFolderID:   in.DriveFolderID,
		CredentialsFile: in.CredentialsFile,
	}
	normalized, err := normalizeStorage(next, false)
	if err != nil {
		return err
	}
	// Clear when no identity (empty input or stripped).
	if !storageHasIdentity(normalized) {
		normalized = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Storage = normalized
	return c.saveLocked()
}

// SetProjectStorage sets a full override. Requires identity after normalize.
// Empty identity → error naming ClearProjectStorage / SetProjectStorageDisabled.
func (c *Config) SetProjectStorage(project string, in StorageInput) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project name is required")
	}
	next := &StorageConfig{
		Backend:         in.Backend,
		GCSBucket:       in.GCSBucket,
		Prefix:          in.Prefix,
		DriveFolderID:   in.DriveFolderID,
		CredentialsFile: in.CredentialsFile,
	}
	// Pre-normalize so empty identity fails with a clear message (not "required" from normalize).
	normalized, err := normalizeStorage(next, true)
	if err != nil {
		return err
	}
	if !storageHasIdentity(normalized) {
		return fmt.Errorf("storage identity is required; use ClearProjectStorage to re-inherit/unlink, or SetProjectStorageDisabled to turn Files off")
	}
	return c.mutateProjectStorage(project, func(_ *StorageConfig) (*StorageConfig, error) {
		return normalized, nil
	})
}

// SetGlobalStorageGCS sets or clears the host default (GCS backend).
// Empty bucket clears (nils c.Storage). Disabled is never accepted.
func (c *Config) SetGlobalStorageGCS(bucket, prefix, credentialsFile string) error {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		// Explicit clear: do not send backend=gcs with empty identity (normalize would error).
		return c.SetGlobalStorage(StorageInput{})
	}
	return c.SetGlobalStorage(StorageInput{
		Backend:         StorageBackendGCS,
		GCSBucket:       bucket,
		Prefix:          prefix,
		CredentialsFile: credentialsFile,
	})
}

// SetProjectStorageGCS sets a full GCS override. Requires a non-empty bucket.
// Empty bucket is an error — use ClearProjectStorage or SetProjectStorageDisabled.
func (c *Config) SetProjectStorageGCS(project, bucket, prefix, credentialsFile string) error {
	if strings.TrimSpace(bucket) == "" {
		return fmt.Errorf("gcsBucket is required; use ClearProjectStorage to re-inherit/unlink, or SetProjectStorageDisabled to turn Files off")
	}
	return c.SetProjectStorage(project, StorageInput{
		Backend:         StorageBackendGCS,
		GCSBucket:       bucket,
		Prefix:          prefix,
		CredentialsFile: credentialsFile,
	})
}

// SetGlobalStorageDrive sets the host default to a Drive folder.
func (c *Config) SetGlobalStorageDrive(folderID, credentialsFile string) error {
	return c.SetGlobalStorage(StorageInput{
		Backend:         StorageBackendGDrive,
		DriveFolderID:   folderID,
		CredentialsFile: credentialsFile,
	})
}

// SetProjectStorageDrive sets a full Drive override.
func (c *Config) SetProjectStorageDrive(project, folderID, credentialsFile string) error {
	return c.SetProjectStorage(project, StorageInput{
		Backend:         StorageBackendGDrive,
		DriveFolderID:   folderID,
		CredentialsFile: credentialsFile,
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
