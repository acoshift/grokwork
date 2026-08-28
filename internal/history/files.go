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
