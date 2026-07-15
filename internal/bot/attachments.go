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

type savedAttachment struct {
	Path        string
	Filename    string
	ContentType string
	Size        int64
}

// downloadAttachments writes files under destDir. Callers must RemoveAll(destDir)
// even after partial failure.
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

	// +1 detects oversize when Discord Size is wrong/missing.
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

func promptWithReferenced(userPrompt string, ref *discordgo.Message) string {
	userPrompt = strings.TrimSpace(userPrompt)
	if ref == nil {
		return userPrompt
	}

	var b strings.Builder
	if userPrompt != "" {
		b.WriteString(userPrompt)
		b.WriteString("\n\n")
	} else {
		b.WriteString("Please review the referenced Discord message.\n\n")
	}

	author := "unknown"
	if ref.Author != nil {
		author = ref.Author.DisplayName()
	}
	fmt.Fprintf(&b, "The user is replying to this earlier Discord message from %s", author)
	if ref.ID != "" {
		fmt.Fprintf(&b, " (id=%s)", ref.ID)
	}
	b.WriteString(":\n")

	content := messagePromptText(ref)
	if content != "" {
		b.WriteString("---\n")
		b.WriteString(content)
		b.WriteString("\n---\n")
	} else {
		b.WriteString("(no text content)\n")
	}
	if n := len(ref.Attachments); n > 0 {
		fmt.Fprintf(&b, "Referenced message has %d attachment(s); files are listed below when downloaded.\n", n)
	}
	return strings.TrimSpace(b.String())
}

func collectAttachments(primary []*discordgo.MessageAttachment, related *discordgo.Message) []*discordgo.MessageAttachment {
	seen := map[string]struct{}{}
	out := make([]*discordgo.MessageAttachment, 0, len(primary)+4)
	add := func(list []*discordgo.MessageAttachment) {
		for _, a := range list {
			if a == nil {
				continue
			}
			key := a.ID
			if key == "" {
				key = a.URL
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}
			out = append(out, a)
		}
	}
	add(primary)
	if related != nil {
		add(related.Attachments)
	}
	return out
}

func resolveReferencedMessage(s *discordgo.Session, m *discordgo.MessageCreate) (*discordgo.Message, error) {
	if m == nil {
		return nil, nil
	}
	if m.ReferencedMessage != nil {
		return m.ReferencedMessage, nil
	}
	if m.MessageReference == nil || m.MessageReference.MessageID == "" {
		return nil, nil
	}
	// Gateway may omit ReferencedMessage; fetch via REST.
	channelID := m.MessageReference.ChannelID
	if channelID == "" {
		channelID = m.ChannelID
	}
	msg, err := s.ChannelMessage(channelID, m.MessageReference.MessageID)
	if err != nil {
		return nil, fmt.Errorf("fetch referenced message %s: %w", m.MessageReference.MessageID, err)
	}
	return msg, nil
}

func hasMessageReference(m *discordgo.MessageCreate) bool {
	return m != nil && m.MessageReference != nil && m.MessageReference.MessageID != ""
}

func sanitizeFilename(name string) string {
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
