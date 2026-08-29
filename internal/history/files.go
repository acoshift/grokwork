package history

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenFile opens a persisted turn attachment. turnN is 1-based (matching the
// session page's #N). name must equal an Attachment.Name on that turn — a file
// that happens to sit in the dir but is not in the JSON is not served.
func (s *Store) OpenFile(threadID string, turnN int, name string) (*os.File, Attachment, error) {
	if s == nil {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	if !validThreadID(threadID) {
		return nil, Attachment{}, fmt.Errorf("invalid thread id")
	}
	name = safeFileName(name)
	if name == "" || turnN < 1 {
		return nil, Attachment{}, fmt.Errorf("not found")
	}

	s.mu.Lock()
	th, err := s.loadLocked(threadID)
	s.mu.Unlock()
	if err != nil {
		return nil, Attachment{}, err
	}
	if turnN > len(th.Turns) {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	att, ok := lookupAttachment(th.Turns[turnN-1].Attachments, name)
	if !ok {
		return nil, Attachment{}, fmt.Errorf("not found")
	}

	dir := s.filesDir(threadID, turnN)
	path := filepath.Join(dir, name)
	if filepath.Dir(path) != dir {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, Attachment{}, err
	}
	return f, att, nil
}

func (s *Store) filesRoot(threadID string) string {
	return filepath.Join(s.dir, threadID)
}

func (s *Store) filesDir(threadID string, turnN int) string {
	return filepath.Join(s.filesRoot(threadID), strconv.Itoa(turnN))
}

func (s *Store) artifactsDir(threadID string) string {
	return filepath.Join(s.filesRoot(threadID), "out")
}

const (
	maxArtifactBytes        = 25 << 20
	maxArtifactsPerSession  = 50
	maxArtifactBytesSession = 200 << 20
)

// PutArtifact copies src into the session artifact store and records it on
// the thread allowlist. Name collisions get a _N suffix. Host-absolute Rel
// values are dropped — only a worktree-relative label is stored.
func (s *Store) PutArtifact(threadID string, src File) (Attachment, error) {
	if s == nil {
		return Attachment{}, fmt.Errorf("not found")
	}
	if !validThreadID(threadID) {
		return Attachment{}, fmt.Errorf("invalid thread id")
	}
	name := safeFileName(src.Name)
	if name == "" {
		name = safeFileName(filepath.Base(src.Path))
	}
	if name == "" {
		return Attachment{}, fmt.Errorf("empty file name")
	}
	size := int64(len(src.Bytes))
	if size == 0 && src.Path != "" {
		st, err := os.Stat(src.Path)
		if err != nil {
			return Attachment{}, err
		}
		if st.IsDir() {
			return Attachment{}, fmt.Errorf("path is a directory, not a file")
		}
		size = st.Size()
	}
	if size > maxArtifactBytes {
		return Attachment{}, fmt.Errorf("file is %s (max %s)", formatFileSize(size), formatFileSize(maxArtifactBytes))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	th, err := s.loadLocked(threadID)
	if err != nil {
		return Attachment{}, err
	}
	if th.ThreadID == "" {
		th.ThreadID = threadID
	}
	if len(th.Artifacts) >= maxArtifactsPerSession {
		return Attachment{}, fmt.Errorf("session already has %d files (max %d)", maxArtifactsPerSession, maxArtifactsPerSession)
	}
	var total int64
	taken := make(map[string]struct{}, len(th.Artifacts)+1)
	for _, a := range th.Artifacts {
		total += a.Size
		taken[a.Name] = struct{}{}
	}
	if total+size > maxArtifactBytesSession {
		return Attachment{}, fmt.Errorf("session file budget reached")
	}
	name = unusedFileName(name, taken)

	dir := s.artifactsDir(threadID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, err
	}
	dest := filepath.Join(dir, name)
	if filepath.Dir(dest) != dir {
		return Attachment{}, fmt.Errorf("not found")
	}
	n, err := writeArtifactFile(dest, src)
	if err != nil {
		return Attachment{}, err
	}
	att := Attachment{
		Name:        name,
		ContentType: strings.TrimSpace(src.ContentType),
		Size:        n,
		Rel:         safeRelLabel(src.Rel),
	}
	th.Artifacts = append(th.Artifacts, att)
	if err := s.saveLocked(th); err != nil {
		_ = os.Remove(dest)
		return Attachment{}, err
	}
	return att, nil
}

// OpenArtifact opens a persisted session artifact. name must equal an
// Attachment.Name on the thread allowlist — a file that happens to sit in
// the dir but is not in the JSON is not served.
func (s *Store) OpenArtifact(threadID, name string) (*os.File, Attachment, error) {
	if s == nil {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	if !validThreadID(threadID) {
		return nil, Attachment{}, fmt.Errorf("invalid thread id")
	}
	name = safeFileName(name)
	if name == "" {
		return nil, Attachment{}, fmt.Errorf("not found")
	}

	s.mu.Lock()
	th, err := s.loadLocked(threadID)
	s.mu.Unlock()
	if err != nil {
		return nil, Attachment{}, err
	}
	att, ok := lookupAttachment(th.Artifacts, name)
	if !ok {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	dir := s.artifactsDir(threadID)
	path := filepath.Join(dir, name)
	if filepath.Dir(path) != dir {
		return nil, Attachment{}, fmt.Errorf("not found")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, Attachment{}, err
	}
	return f, att, nil
}

func lookupAttachment(atts []Attachment, name string) (Attachment, bool) {
	for _, a := range atts {
		if a.Name == name {
			return a, true
		}
	}
	return Attachment{}, false
}

func copyTurnFiles(dir string, files []File) []Attachment {
	if len(files) == 0 {
		return nil
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	used := map[string]int{}
	out := make([]Attachment, 0, len(files))
	for _, f := range files {
		name := safeFileName(f.Name)
		if name == "" {
			name = safeFileName(filepath.Base(f.Path))
		}
		if name == "" {
			continue
		}
		name = uniqueFileName(name, used)
		dest := filepath.Join(dir, name)
		n, err := copyFile(f.Path, dest)
		if err != nil {
			continue
		}
		out = append(out, Attachment{
			Name:        name,
			ContentType: strings.TrimSpace(f.ContentType),
			Size:        n,
		})
	}
	if len(out) == 0 {
		_ = os.RemoveAll(dir)
		return nil
	}
	return out
}

func writeArtifactFile(dst string, src File) (int64, error) {
	if len(src.Bytes) > 0 {
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return 0, err
		}
		n, err := out.Write(src.Bytes)
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
			return 0, err
		}
		return int64(n), nil
	}
	return copyFile(src.Path, dst)
}

func safeRelLabel(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return ""
	}
	var out []string
	for part := range strings.SplitSeq(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return ""
		}
		if strings.Contains(part, ":") {
			return ""
		}
		out = append(out, part)
	}
	return strings.Join(out, "/")
}

func formatFileSize(n int64) string {
	return Attachment{Size: n}.SizeLabel()
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dst)
		return 0, err
	}
	return n, nil
}

func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "" || name == string(filepath.Separator) {
		return ""
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return ""
	}
	return name
}

func uniqueFileName(name string, used map[string]int) string {
	n := used[name]
	used[name] = n + 1
	if n == 0 {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s_%d%s", base, n+1, ext)
}

func unusedFileName(name string, taken map[string]struct{}) string {
	if _, ok := taken[name]; !ok {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, ok := taken[cand]; !ok {
			return cand
		}
	}
}

func mediaType(ctype string) string {
	ct := strings.ToLower(strings.TrimSpace(ctype))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}

// IsImage reports whether the session page should inline this file as an <img>.
// SVG is excluded: served as a download, not executed as markup.
func (a Attachment) IsImage() bool {
	ct := mediaType(a.ContentType)
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	if ct == "" || ct == "application/octet-stream" {
		switch strings.ToLower(filepath.Ext(a.Name)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			return true
		}
	}
	return false
}

// SizeLabel is a compact size for chips; empty when Size is unknown.
func (a Attachment) SizeLabel() string {
	if a.Size <= 0 {
		return ""
	}
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case a.Size >= mb:
		return fmt.Sprintf("%.1f MiB", float64(a.Size)/float64(mb))
	case a.Size >= kb:
		return fmt.Sprintf("%.1f KiB", float64(a.Size)/float64(kb))
	default:
		return fmt.Sprintf("%d B", a.Size)
	}
}
