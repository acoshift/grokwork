// Package gcs wraps the gcloud storage CLI for project file storage.
package gcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Binary is the gcloud executable name. Tests PATH-poison gcloud so the real
// binary can never be exec'd from tests.
const Binary = "gcloud"

// maxObjectPathBytes is the GCS object-name limit and our validation ceiling.
const maxObjectPathBytes = 1024

// Runner runs a command in dir with extra env vars appended to the host
// environment, and returns stdout. Tests inject fakes. env is how per-project
// credentials reach gcloud — it is part of the seam so tests can assert it.
type Runner func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

var defaultRunner Runner = execRunner

func execRunner(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.Bytes()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

func runOrDefault(run Runner) Runner {
	if run != nil {
		return run
	}
	return defaultRunner
}

// Target names the bucket every operation acts on, the optional object-name
// prefix under it, and the optional service-account key that authenticates the
// call. One struct rather than three positional strings: bucket, prefix and a
// key path are all strings, and a swapped pair compiles.
type Target struct {
	Bucket string
	// Prefix is an optional object-name prefix (validated at config write;
	// re-validated at this boundary).
	Prefix string
	// CredentialsFile is an optional absolute path to a service-account JSON
	// key. Empty means the host's gcloud login/ADC.
	CredentialsFile string
}

// validate is the shared precondition for every operation.
func (t Target) validate() error {
	if strings.TrimSpace(t.Bucket) == "" {
		return fmt.Errorf("bucket is required")
	}
	if err := ValidateObjectPath(t.Prefix); err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	return nil
}

// env returns the per-invocation environment for the target.
// CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE is what makes gcloud use the key for
// this call only — activate-service-account would rewrite the host's global
// gcloud auth for every other process on the box.
// GOOGLE_APPLICATION_CREDENTIALS rides along for any ADC-reading code path.
func (t Target) env() []string {
	f := strings.TrimSpace(t.CredentialsFile)
	if f == "" {
		return nil
	}
	return []string{
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + f,
		"GOOGLE_APPLICATION_CREDENTIALS=" + f,
	}
}

// Entry is one object or folder pseudo-entry from a listing or describe.
type Entry struct {
	// Name is relative to the list's prefix/subPath (or the bare object leaf for Describe).
	Name        string
	IsDir       bool
	Size        int64
	Updated     time.Time
	ContentType string
}

// ValidateObjectPath rejects names that would escape the prefix, expand as
// wildcards under gcloud, or inject control characters. Empty is allowed so
// callers can treat "" as the bucket root; object operations refuse it after.
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
	if strings.ContainsAny(p, "*?[]") {
		return fmt.Errorf("object path must not contain wildcard characters")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return fmt.Errorf("object path must not contain control characters")
		}
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("object path has an empty or '.'/'..' segment")
		}
	}
	return nil
}

// SanitizeFilename cleans a client-supplied filename for use as an object leaf.
// Mirrors bot.sanitizeFilename without importing internal/bot.
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

// requireObject rejects an empty object argument. joinObject(prefix, "") is
// non-empty whenever a prefix is set, so checking the joined path would let an
// empty object name address the prefix itself — the check must be on the
// argument, before joining.
func requireObject(object string) error {
	if strings.Trim(strings.TrimSpace(object), "/") == "" {
		return fmt.Errorf("object path is required")
	}
	return nil
}

// joinObject builds a path under the bucket from optional segments, without a
// leading or trailing slash. path.Join would collapse ".." — segments are
// validated first so that never reaches the CLI.
func joinObject(parts ...string) string {
	var segs []string
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			segs = append(segs, p)
		}
	}
	return path.Join(segs...)
}

// objectURL builds gs://bucket[/object]. Callers must have validated object.
func objectURL(bucket, object string) string {
	bucket = strings.TrimSpace(bucket)
	object = strings.Trim(object, "/")
	if object == "" {
		return "gs://" + bucket + "/"
	}
	return "gs://" + bucket + "/" + object
}

// listURL is objectURL with a trailing slash so gcloud lists one level under it.
func listURL(bucket, object string) string {
	u := objectURL(bucket, object)
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// List returns one level of entries under the target's prefix + subPath.
// run may be nil (production default).
func List(ctx context.Context, run Runner, t Target, subPath string) ([]Entry, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if err := ValidateObjectPath(subPath); err != nil {
		return nil, err
	}
	under := joinObject(t.Prefix, subPath)
	url := listURL(t.Bucket, under)
	args := []string{"storage", "ls", "--json", url}
	out, err := runOrDefault(run)(ctx, "", t.env(), Binary, args...)
	if err != nil {
		return nil, err
	}
	return parseListJSON(out, under)
}

// Describe returns metadata for one object. exists is false when the object is
// missing (not an error). run may be nil (production default).
func Describe(ctx context.Context, run Runner, t Target, object string) (Entry, bool, error) {
	if err := t.validate(); err != nil {
		return Entry{}, false, err
	}
	if err := ValidateObjectPath(object); err != nil {
		return Entry{}, false, err
	}
	if err := requireObject(object); err != nil {
		return Entry{}, false, err
	}
	full := joinObject(t.Prefix, object)
	url := objectURL(t.Bucket, full)
	args := []string{"storage", "objects", "describe", url, "--format=json"}
	out, err := runOrDefault(run)(ctx, "", t.env(), Binary, args...)
	if err != nil {
		if isNotFound(err) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	e, err := parseDescribeJSON(out, full)
	if err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// Upload copies localPath to gs://bucket/prefix/object. run may be nil
// (production default).
func Upload(ctx context.Context, run Runner, localPath string, t Target, object string) error {
	if strings.TrimSpace(localPath) == "" {
		return fmt.Errorf("local path is required")
	}
	if err := t.validate(); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	if err := requireObject(object); err != nil {
		return err
	}
	full := joinObject(t.Prefix, object)
	url := objectURL(t.Bucket, full)
	args := []string{"storage", "cp", localPath, url}
	_, err := runOrDefault(run)(ctx, "", t.env(), Binary, args...)
	return err
}

// Download copies gs://bucket/prefix/object to destPath. run may be nil
// (production default).
func Download(ctx context.Context, run Runner, t Target, object, destPath string) error {
	if strings.TrimSpace(destPath) == "" {
		return fmt.Errorf("destination path is required")
	}
	if err := t.validate(); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	if err := requireObject(object); err != nil {
		return err
	}
	full := joinObject(t.Prefix, object)
	url := objectURL(t.Bucket, full)
	args := []string{"storage", "cp", url, destPath}
	_, err := runOrDefault(run)(ctx, "", t.env(), Binary, args...)
	return err
}

// Delete removes one object. Refuses wildcards, trailing slashes, and never
// passes -r. run may be nil (production default).
func Delete(ctx context.Context, run Runner, t Target, object string) error {
	if err := t.validate(); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	if err := requireObject(object); err != nil {
		return err
	}
	full := joinObject(t.Prefix, object)
	url := objectURL(t.Bucket, full)
	// Belt-and-braces: never delete a "folder" or a wildcard URL even if a
	// future caller bypasses ValidateObjectPath.
	if strings.ContainsAny(url, "*?[]") {
		return fmt.Errorf("refusing delete: URL contains a wildcard character")
	}
	if strings.HasSuffix(url, "/") {
		return fmt.Errorf("refusing delete: URL ends with / (not a single object)")
	}
	args := []string{"storage", "rm", url}
	_, err := runOrDefault(run)(ctx, "", t.env(), Binary, args...)
	return err
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "404") ||
		strings.Contains(s, "no such object") ||
		strings.Contains(s, "does not exist")
}

// lsItem is one element of `gcloud storage ls --json`.
type lsItem struct {
	URL      string          `json:"url"`
	Type     string          `json:"type"`
	Metadata json.RawMessage `json:"metadata"`
}

// objectMeta is the subset of GCS object metadata we care about.
type objectMeta struct {
	Name        string          `json:"name"`
	Size        json.RawMessage `json:"size"`
	Updated     string          `json:"updated"`
	TimeCreated string          `json:"timeCreated"`
	ContentType string          `json:"contentType"`
	// Some describe payloads nest under different keys; tolerate both.
	ContentTypeAlt string `json:"content_type"`
}

func parseListJSON(raw []byte, under string) ([]Entry, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var items []lsItem
	if err := json.Unmarshal(raw, &items); err != nil {
		// A single object describe-shaped payload is not expected from ls, but
		// tolerate a bare array failure with a clear error.
		return nil, fmt.Errorf("parse ls json: %w", err)
	}
	under = strings.Trim(under, "/")
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		e, ok := entryFromListItem(it, under)
		if !ok {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func entryFromListItem(it lsItem, under string) (Entry, bool) {
	u := strings.TrimSpace(it.URL)
	if u == "" {
		return Entry{}, false
	}
	// Strip gs://bucket/ to get the full object path.
	full := objectPathFromURL(u)
	isDir := strings.HasSuffix(u, "/") || strings.EqualFold(it.Type, "prefix")
	rel := relativeName(full, under)
	if rel == "" {
		return Entry{}, false
	}
	e := Entry{Name: rel, IsDir: isDir}
	if len(it.Metadata) > 0 && !isDir {
		var meta objectMeta
		if err := json.Unmarshal(it.Metadata, &meta); err == nil {
			fillEntryMeta(&e, meta)
		}
	}
	return e, true
}

func parseDescribeJSON(raw []byte, full string) (Entry, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return Entry{}, fmt.Errorf("empty describe response")
	}
	var meta objectMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Entry{}, fmt.Errorf("parse describe json: %w", err)
	}
	name := path.Base(strings.Trim(full, "/"))
	if meta.Name != "" {
		name = path.Base(meta.Name)
	}
	e := Entry{Name: name}
	fillEntryMeta(&e, meta)
	return e, nil
}

func fillEntryMeta(e *Entry, meta objectMeta) {
	e.Size = parseSize(meta.Size)
	e.ContentType = strings.TrimSpace(meta.ContentType)
	if e.ContentType == "" {
		e.ContentType = strings.TrimSpace(meta.ContentTypeAlt)
	}
	if t := parseTime(meta.Updated); !t.IsZero() {
		e.Updated = t
	} else if t := parseTime(meta.TimeCreated); !t.IsZero() {
		e.Updated = t
	}
}

func parseSize(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	// Number.
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	// String (GCS JSON API often returns size as a string).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// objectPathFromURL extracts the object path from gs://bucket/path.
func objectPathFromURL(u string) string {
	u = strings.TrimSpace(u)
	const pfx = "gs://"
	if !strings.HasPrefix(u, pfx) {
		return strings.Trim(u, "/")
	}
	rest := u[len(pfx):]
	_, obj, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return strings.Trim(obj, "/")
}

// relativeName returns the path relative to under. For directories the trailing
// slash is stripped from the display name but IsDir is already set.
func relativeName(full, under string) string {
	full = strings.Trim(full, "/")
	under = strings.Trim(under, "/")
	if under != "" {
		if full == under {
			return ""
		}
		pref := under + "/"
		if !strings.HasPrefix(full, pref) {
			// Fall back to the leaf when the URL is not under the list path
			// (defensive — should not happen for a correct listing).
			return path.Base(full)
		}
		full = strings.TrimPrefix(full, pref)
	}
	// One level only: a nested path should not come back from a non-recursive
	// ls; if one does, keep the first segment (the folder or file name).
	seg, _, _ := strings.Cut(full, "/")
	return seg
}
