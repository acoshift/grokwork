package bot

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoshift/grokwork/internal/runjournal"
)

// WebUpload is one file posted through the web UI.
type WebUpload struct {
	Filename string
	Size     int64
	Open     func() (io.ReadCloser, error)
}

func webStagingRoot(dataDir string) string {
	return filepath.Join(dataDir, "attachments", "web")
}

// SaveWebAttachments writes uploads into a fresh staging dir under
// <DataDir>/attachments/web/<id>/ and returns the saved paths.
// cleanup removes the staging dir; callers invoke it when the task
// hand-off fails (on success the bot owns the files).
func (b *Bot) SaveWebAttachments(uploads []WebUpload) (paths []string, cleanup func(), err error) {
	noop := func() {}
	if len(uploads) == 0 {
		return nil, noop, nil
	}
	if b == nil || b.cfg == nil || strings.TrimSpace(b.cfg.DataDir) == "" {
		return nil, noop, fmt.Errorf("bot data dir not configured")
	}
	if len(uploads) > maxAttachments {
		return nil, noop, fmt.Errorf("too many attachments (%d); max is %d", len(uploads), maxAttachments)
	}

	var declaredTotal int64
	for _, u := range uploads {
		if u.Size > maxAttachmentBytes {
			return nil, noop, fmt.Errorf("attachment %q is %s; max per file is %s",
				u.Filename, formatBytes(u.Size), formatBytes(maxAttachmentBytes))
		}
		if u.Size > 0 {
			declaredTotal += u.Size
		}
	}
	if declaredTotal > maxTotalBytes {
		return nil, noop, fmt.Errorf("attachments total %s; max is %s", formatBytes(declaredTotal), formatBytes(maxTotalBytes))
	}

	destDir := filepath.Join(webStagingRoot(b.cfg.DataDir), runjournal.NewTaskID())
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, noop, fmt.Errorf("create attachment dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(destDir) }
	fail := func(err error) ([]string, func(), error) {
		cleanup()
		return nil, noop, err
	}

	usedNames := map[string]int{}
	out := make([]string, 0, len(uploads))
	var total int64

	for _, u := range uploads {
		name := uniqueFilename(sanitizeFilename(u.Filename), usedNames)
		path := filepath.Join(destDir, name)

		if u.Open == nil {
			return fail(fmt.Errorf("attachment %q has no reader", u.Filename))
		}
		rc, openErr := u.Open()
		if openErr != nil {
			return fail(fmt.Errorf("open %q: %w", u.Filename, openErr))
		}

		n, writeErr := saveWebFile(u.Filename, rc, path, maxAttachmentBytes)
		_ = rc.Close()
		if writeErr != nil {
			return fail(writeErr)
		}
		total += n
		if total > maxTotalBytes {
			return fail(fmt.Errorf("attachments total %s; max is %s", formatBytes(total), formatBytes(maxTotalBytes)))
		}
		out = append(out, path)
	}
	return out, cleanup, nil
}

// saveWebFile writes r under dest with a per-file size guard. Type is not
// gated — Discord attachments already accept PDFs and other files; the web
// composers match that.
func saveWebFile(name string, r io.Reader, dest string, maxBytes int64) (int64, error) {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	if err != nil {
		_ = os.Remove(dest)
		return 0, fmt.Errorf("write %q: %w", name, err)
	}
	if n > maxBytes {
		_ = os.Remove(dest)
		return 0, fmt.Errorf("file exceeds %s limit", formatBytes(maxBytes))
	}
	return n, nil
}

// removeWebStagedAttachments best-effort removes paths under the web staging root
// and then the parent staging dir when empty. Paths outside the root are ignored.
func removeWebStagedAttachments(dataDir string, paths []string) {
	if dataDir == "" || len(paths) == 0 {
		return
	}
	root := filepath.Clean(webStagingRoot(dataDir))
	parents := map[string]struct{}{}
	for _, p := range paths {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." {
			continue
		}
		if !isUnderDir(root, p) {
			continue
		}
		_ = os.Remove(p)
		parents[filepath.Dir(p)] = struct{}{}
	}
	for dir := range parents {
		if isUnderDir(root, dir) || dir == root {
			_ = os.Remove(dir) // only succeeds when empty
		}
	}
}

func isUnderDir(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return false
	}
	prefix := root + string(filepath.Separator)
	return strings.HasPrefix(path, prefix)
}
