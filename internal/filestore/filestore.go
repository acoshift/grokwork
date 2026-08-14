// Package filestore is the backend-agnostic surface for project Files storage.
package filestore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	BackendGCS    = "gcs"
	BackendGDrive = "gdrive"
)

// maxObjectPathBytes is the shared validation ceiling (GCS object-name limit).
const maxObjectPathBytes = 1024

// Entry is one listing or describe row.
type Entry struct {
	Name        string
	IsDir       bool
	Size        int64
	Updated     time.Time
	ContentType string
}

// Target is the resolved identity for one Files operation (from EffectiveStorage).
type Target struct {
	Backend string // "gcs" | "gdrive"

	// GCS
	Bucket string
	Prefix string

	// Drive
	FolderID         string
	IsolationSegment string

	CredentialsFile string
}

// Backend is the storage operations surface used by the Files page.
type Backend interface {
	List(ctx context.Context, t Target, subPath string) ([]Entry, error)
	Describe(ctx context.Context, t Target, object string) (Entry, bool, error)
	Upload(ctx context.Context, localPath string, t Target, object string, overwrite bool) error
	Download(ctx context.Context, t Target, object, destPath string) error
	Delete(ctx context.Context, t Target, object string) error
}

// ValidateObjectPath rejects names that would escape the prefix or inject
// control characters. Empty is allowed so callers can treat "" as the root.
// Wildcard characters (*?[]) are allowed here — Drive display names use them.
// GCS still refuses them at the gcloud argv boundary (internal/gcs).
// A hop may contain '/' when encoded as %2F (one Drive/GCS display name).
func ValidateObjectPath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("object path must not start with /")
	}
	if len(p) > maxObjectPathBytes {
		return fmt.Errorf("object path exceeds %d bytes", maxObjectPathBytes)
	}
	segs, err := SplitPath(p)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("object path has an empty or '.'/'..' segment")
	}
	for _, name := range segs {
		if err := validDecodedName(name); err != nil {
			return err
		}
	}
	return nil
}

// SanitizeFilename cleans a client-supplied filename for use as an object leaf.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		name = "file"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name = strings.TrimSpace(b.String())
	name = strings.Trim(name, ".")
	if name == "" {
		name = "file"
	}
	const max = 120
	if len(name) > max {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		if len(ext) > 20 {
			ext = ext[:20]
		}
		keep := max - len(ext)
		if keep < 1 {
			keep = 1
		}
		if len(base) > keep {
			base = base[:keep]
		}
		name = base + ext
	}
	return name
}
