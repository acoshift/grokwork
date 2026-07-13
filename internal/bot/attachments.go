package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

const (
	maxAttachments     = 10
	maxAttachmentBytes = 25 << 20 // 25 MiB per file
	maxTotalBytes      = 50 << 20 // 50 MiB total
	downloadTimeout    = 60 * time.Second
)

// savedAttachment is one Discord file written to disk for a Grok run.
type savedAttachment struct {
	Path        string
	Filename    string
	ContentType string
	Size        int64
}

// downloadAttachments saves Discord attachments under destDir.
// Returns the list of saved files (may be empty). On partial failure after
// writing some files, destDir may still contain those files — caller should
// always RemoveAll(destDir) when done.
func downloadAttachments(ctx context.Context, attachments []*discordgo.MessageAttachment, destDir string) ([]savedAttachment, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if len(attachments) > maxAttachments {
		return nil, fmt.Errorf("too many attachments (%d); max is %d", len(attachments), maxAttachments)
	}

	var total int64
	for _, a := range attachments {
		if a == nil {
			continue
		}
		if a.Size > maxAttachmentBytes {
			return nil, fmt.Errorf("attachment %q is %s; max per file is %s",
				a.Filename, formatBytes(int64(a.Size)), formatBytes(maxAttachmentBytes))
		}
		total += int64(a.Size)
	}
	if total > maxTotalBytes {
		return nil, fmt.Errorf("attachments total %s; max is %s", formatBytes(total), formatBytes(maxTotalBytes))
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment dir: %w", err)
	}

	client := &http.Client{Timeout: downloadTimeout}
	usedNames := map[string]int{}
	out := make([]savedAttachment, 0, len(attachments))

	for _, a := range attachments {
		if a == nil {
			continue
		}
		if a.URL == "" {
			return out, fmt.Errorf("attachment %q has no URL", a.Filename)
		}

		name := uniqueFilename(sanitizeFilename(a.Filename), usedNames)
		path := filepath.Join(destDir, name)

		n, ctype, err := downloadOne(ctx, client, a.URL, path, maxAttachmentBytes)
		if err != nil {
			return out, fmt.Errorf("download %q: %w", a.Filename, err)
		}
		if ctype == "" {
			ctype = a.ContentType
		}
		out = append(out, savedAttachment{
			Path:        path,
			Filename:    name,
			ContentType: ctype,
			Size:        n,
		})
	}
	return out, nil
}

func downloadOne(ctx context.Context, client *http.Client, url, dest string, maxBytes int64) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	// +1 so we can detect oversize streams when Discord Size is wrong/missing.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		_ = os.Remove(dest)
		return 0, "", err
	}
	if n > maxBytes {
		_ = os.Remove(dest)
		return 0, "", fmt.Errorf("file exceeds %s limit", formatBytes(maxBytes))
	}
	return n, resp.Header.Get("Content-Type"), nil
}

// promptWithAttachments appends a file list the model can open with tools.
func promptWithAttachments(userPrompt string, files []savedAttachment) string {
	userPrompt = strings.TrimSpace(userPrompt)
	if len(files) == 0 {
		return userPrompt
	}

	var b strings.Builder
	if userPrompt != "" {
		b.WriteString(userPrompt)
		b.WriteString("\n\n")
	} else {
		b.WriteString("Please review the attached files.\n\n")
	}
	b.WriteString("Attached files from Discord (read these paths with your tools):\n")
	for _, f := range files {
		ctype := f.ContentType
		if ctype == "" {
			ctype = "unknown"
		}
		fmt.Fprintf(&b, "- %s (%s, %s)\n", f.Path, ctype, formatBytes(f.Size))
	}
	return strings.TrimSpace(b.String())
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		name = "file"
	}
	// Keep a conservative set of characters for cross-platform paths.
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
	// Cap length but keep extension when possible.
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

func uniqueFilename(name string, used map[string]int) string {
	n := used[name]
	used[name] = n + 1
	if n == 0 {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s_%d%s", base, n+1, ext)
}

func formatBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
