// Package projstore is a project-scoped local blob store for agent control plane storage tools.
package projstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/acoshift/grokwork/internal/atomicfile"
)

const (
	// MaxKeyLen is the max storage key length.
	MaxKeyLen = 200
	// DefaultMaxObjectBytes is 25 MiB (aligned with Discord upload caps).
	DefaultMaxObjectBytes = 25 << 20
	// DefaultMaxProjectBytes is 1 GiB per project.
	DefaultMaxProjectBytes = 1 << 30
)

// Meta describes one stored object.
type Meta struct {
	Key               string    `json:"key"`
	Size              int64     `json:"size"`
	ContentType       string    `json:"contentType,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	CreatedBySession  string    `json:"createdBySession,omitempty"`
	CreatedByActor    string    `json:"createdByActor,omitempty"`
	UpdatedBySession  string    `json:"updatedBySession,omitempty"`
	UpdatedByActor    string    `json:"updatedByActor,omitempty"`
}

// Store roots at data/storage.
type Store struct {
	root           string
	maxObjectBytes int64
	maxProjectBytes int64
	mu             sync.Mutex
}

// New returns a store under dataDir/storage.
func New(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "storage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{
		root:            root,
		maxObjectBytes:  DefaultMaxObjectBytes,
		maxProjectBytes: DefaultMaxProjectBytes,
	}, nil
}

// ValidateKey rejects path escape and illegal keys.
func ValidateKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("key too long")
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return fmt.Errorf("invalid key")
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid key segment")
		}
		for _, r := range seg {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '.' || r == '_' || r == '-' {
				continue
			}
			return fmt.Errorf("invalid key character %q", r)
		}
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("invalid key encoding")
	}
	return nil
}

// Put writes content for project/key.
func (s *Store) Put(project, key string, content []byte, contentType, sessionID, actorID string) (Meta, error) {
	if s == nil {
		return Meta{}, fmt.Errorf("nil store")
	}
	project = strings.TrimSpace(project)
	if project == "" || strings.Contains(project, "/") || project == ".." || project == "." {
		return Meta{}, fmt.Errorf("invalid project")
	}
	if err := ValidateKey(key); err != nil {
		return Meta{}, err
	}
	key = strings.TrimSpace(key)
	if int64(len(content)) > s.maxObjectBytes {
		return Meta{}, fmt.Errorf("object exceeds max size %d", s.maxObjectBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	projRoot, err := s.projectRoot(project)
	if err != nil {
		return Meta{}, err
	}
	if err := os.MkdirAll(filepath.Join(projRoot, "objects"), 0o700); err != nil {
		return Meta{}, err
	}
	if err := os.MkdirAll(filepath.Join(projRoot, "meta"), 0o700); err != nil {
		return Meta{}, err
	}

	used, err := s.projectBytesLocked(projRoot)
	if err != nil {
		return Meta{}, err
	}
	objPath := s.objectPath(projRoot, key)
	var oldSize int64
	if st, err := os.Stat(objPath); err == nil {
		oldSize = st.Size()
	}
	next := used - oldSize + int64(len(content))
	if next > s.maxProjectBytes {
		return Meta{}, fmt.Errorf("project storage quota exceeded")
	}

	now := time.Now().UTC()
	meta := Meta{
		Key:              key,
		Size:             int64(len(content)),
		ContentType:      strings.TrimSpace(contentType),
		UpdatedAt:        now,
		UpdatedBySession: sessionID,
		UpdatedByActor:   actorID,
	}
	if old, err := s.readMeta(projRoot, key); err == nil {
		meta.CreatedAt = old.CreatedAt
		meta.CreatedBySession = old.CreatedBySession
		meta.CreatedByActor = old.CreatedByActor
	} else {
		meta.CreatedAt = now
		meta.CreatedBySession = sessionID
		meta.CreatedByActor = actorID
	}

	if err := atomicfile.Write(objPath, content, 0o600); err != nil {
		return Meta{}, err
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return Meta{}, err
	}
	if err := atomicfile.Write(s.metaPath(projRoot, key), raw, 0o600); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Get returns content and meta.
func (s *Store) Get(project, key string) ([]byte, Meta, error) {
	if s == nil {
		return nil, Meta{}, fmt.Errorf("nil store")
	}
	if err := ValidateKey(key); err != nil {
		return nil, Meta{}, err
	}
	key = strings.TrimSpace(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	projRoot, err := s.projectRoot(project)
	if err != nil {
		return nil, Meta{}, err
	}
	meta, err := s.readMeta(projRoot, key)
	if err != nil {
		return nil, Meta{}, err
	}
	data, err := os.ReadFile(s.objectPath(projRoot, key))
	if err != nil {
		return nil, Meta{}, err
	}
	return data, meta, nil
}

// List returns metas with optional prefix.
func (s *Store) List(project, prefix string, limit int) ([]Meta, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projRoot, err := s.projectRoot(project)
	if err != nil {
		return nil, err
	}
	metaDir := filepath.Join(projRoot, "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// meta filenames are hashed; load and filter by key prefix.
		raw, err := os.ReadFile(filepath.Join(metaDir, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if prefix != "" && !strings.HasPrefix(m.Key, prefix) {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Delete removes a key.
func (s *Store) Delete(project, key string) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	projRoot, err := s.projectRoot(project)
	if err != nil {
		return err
	}
	_ = os.Remove(s.objectPath(projRoot, key))
	_ = os.Remove(s.metaPath(projRoot, key))
	return nil
}

func (s *Store) projectRoot(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" || strings.Contains(project, string(filepath.Separator)) || project == ".." || project == "." {
		return "", fmt.Errorf("invalid project")
	}
	// Single path element only.
	if filepath.Base(project) != project {
		return "", fmt.Errorf("invalid project")
	}
	root := filepath.Join(s.root, project)
	// Containment: resolved path must stay under s.root.
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if abs != absRoot && !strings.HasPrefix(abs, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escape")
	}
	return abs, nil
}

func (s *Store) objectPath(projRoot, key string) string {
	return filepath.Join(projRoot, "objects", keyHash(key))
}

func (s *Store) metaPath(projRoot, key string) string {
	return filepath.Join(projRoot, "meta", keyHash(key)+".json")
}

func (s *Store) readMeta(projRoot, key string) (Meta, error) {
	raw, err := os.ReadFile(s.metaPath(projRoot, key))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, err
	}
	return m, nil
}

func (s *Store) projectBytesLocked(projRoot string) (int64, error) {
	objDir := filepath.Join(projRoot, "objects")
	entries, err := os.ReadDir(objDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var n int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		n += st.Size()
	}
	return n, nil
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
