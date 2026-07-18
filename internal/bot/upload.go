package bot

import (
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	maxUploadFiles = 10
	maxUploadBytes = 25 << 20 // Discord default ~25 MiB
)

// DISCORD_UPLOAD block forms:
//
//	DISCORD_UPLOAD:
//	- path/to/file.apk
//	path/to/report.xlsx
//
//	DISCORD_UPLOAD: path/to/file.apk
var (
	uploadBlockRE = regexp.MustCompile(`(?im)^[ \t]*DISCORD_UPLOAD:[ \t]*\n((?:[ \t]*(?:[-*•]\s+)?\S+[ \t]*\n?)*)`)
	uploadLineRE  = regexp.MustCompile(`(?im)^[ \t]*DISCORD_UPLOAD:[ \t]+(\S+)[ \t]*$`)
)

// parseUploadPaths extracts file paths from DISCORD_UPLOAD markers in model text.
func parseUploadPaths(text string) []string {
	if text == "" {
		return nil
	}
	var raw []string
	for _, m := range uploadBlockRE.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "-")
			line = strings.TrimPrefix(line, "*")
			line = strings.TrimPrefix(line, "•")
			line = strings.TrimSpace(line)
			// strip surrounding backticks/quotes
			line = strings.Trim(line, "`\"'")
			if line != "" && !strings.EqualFold(line, "DISCORD_UPLOAD:") {
				raw = append(raw, line)
			}
		}
	}
	for _, m := range uploadLineRE.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			p := strings.Trim(strings.TrimSpace(m[1]), "`\"'")
			if p != "" {
				raw = append(raw, p)
			}
		}
	}
	return uniquePreserve(raw)
}

func uniquePreserve(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// resolveWorktreeUploadPath resolves p under worktreeRoot and ensures the
// final path (after EvalSymlinks when possible) stays inside the worktree.
func resolveWorktreeUploadPath(worktreeRoot, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	root, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)

	var candidate string
	if filepath.IsAbs(p) {
		candidate = filepath.Clean(p)
	} else {
		candidate = filepath.Clean(filepath.Join(root, p))
	}

	if !pathInsideRoot(candidate, root) {
		return "", fmt.Errorf("path is outside the worktree: %s", p)
	}

	// Prefer real path so symlinks cannot escape the worktree.
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// File missing or intermediate link issue — still check existence for clearer error.
		if st, stErr := os.Stat(candidate); stErr != nil {
			return "", fmt.Errorf("file not found: %s", p)
		} else if st.IsDir() {
			return "", fmt.Errorf("path is a directory, not a file: %s", p)
		}
		// Stat ok but EvalSymlinks failed (rare); use candidate if still under root.
		return candidate, nil
	}
	real = filepath.Clean(real)
	if !pathInsideRoot(real, root) {
		// Also allow if root itself is a symlink target chain.
		realRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil || !pathInsideRoot(real, filepath.Clean(realRoot)) {
			return "", fmt.Errorf("path escapes the worktree via symlink: %s", p)
		}
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", p)
	}
	if st.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s", p)
	}
	return real, nil
}

func pathInsideRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true // root itself (not useful as file, but "inside")
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(path, root+sep)
}

type preparedUpload struct {
	Path     string
	Name     string
	Size     int64
	MIME     string
	RelLabel string // display path relative to worktree when possible
}

func prepareWorktreeUploads(worktreeRoot string, paths []string) (ok []preparedUpload, notes []string) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > maxUploadFiles {
		notes = append(notes, fmt.Sprintf("only the first %d of %d files will be uploaded", maxUploadFiles, len(paths)))
		paths = paths[:maxUploadFiles]
	}
	root, _ := filepath.Abs(worktreeRoot)
	root = filepath.Clean(root)

	var total int64
	for _, p := range paths {
		abs, err := resolveWorktreeUploadPath(worktreeRoot, p)
		if err != nil {
			notes = append(notes, fmt.Sprintf("skip %q: %v", p, err))
			continue
		}
		st, err := os.Stat(abs)
		if err != nil {
			notes = append(notes, fmt.Sprintf("skip %q: %v", p, err))
			continue
		}
		if st.Size() > maxUploadBytes {
			notes = append(notes, fmt.Sprintf("skip %q: file is %s (max %s)", p, formatBytes(st.Size()), formatBytes(maxUploadBytes)))
			continue
		}
		const maxBatch = 100 << 20 // 100 MiB across one upload batch
		if total+st.Size() > maxBatch {
			notes = append(notes, fmt.Sprintf("skip %q: batch size limit reached", p))
			continue
		}
		total += st.Size()
		name := filepath.Base(abs)
		rel := abs
		if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		ctype := mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		ok = append(ok, preparedUpload{
			Path:     abs,
			Name:     name,
			Size:     st.Size(),
			MIME:     ctype,
			RelLabel: rel,
		})
	}
	return ok, notes
}

// uploadWorktreeFiles posts files from the thread worktree to Discord.
// Only paths under worktreeRoot are allowed. Callers must only invoke when a worktree exists.
func uploadWorktreeFiles(s *discordgo.Session, channelID, worktreeRoot string, text string) {
	paths := parseUploadPaths(text)
	if len(paths) == 0 {
		return
	}
	files, notes := prepareWorktreeUploads(worktreeRoot, paths)
	for _, n := range notes {
		log.Printf("upload: %s", n)
	}
	if len(files) == 0 {
		msg := "Could not upload requested files (must exist inside this thread's worktree, ≤25 MiB each)."
		if len(notes) > 0 {
			msg += "\n" + strings.Join(notes, "\n")
		}
		if _, err := discordSend(s, channelID, msg); err != nil {
			log.Printf("error: upload failure notice: %v", err)
		}
		return
	}

	// Open all readers, then send; close after send.
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	dfiles := make([]*discordgo.File, 0, len(files))
	var names []string
	for _, f := range files {
		r, err := os.Open(f.Path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("skip %q: open: %v", f.RelLabel, err))
			continue
		}
		closers = append(closers, r)
		dfiles = append(dfiles, &discordgo.File{
			Name:        f.Name,
			ContentType: f.MIME,
			Reader:      r,
		})
		names = append(names, fmt.Sprintf("`%s` (%s)", f.RelLabel, formatBytes(f.Size)))
	}
	if len(dfiles) == 0 {
		if _, err := discordSend(s, channelID, "Could not open files for upload:\n"+strings.Join(notes, "\n")); err != nil {
			log.Printf("error: upload open notice: %v", err)
		}
		return
	}

	content := "📎 **Uploaded from worktree**\n" + strings.Join(names, "\n")
	if len(notes) > 0 {
		content += "\n\n" + strings.Join(notes, "\n")
	}
	if len(content) > maxMsg {
		content = truncate(content, maxMsg-1)
	}

	if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		Files:   dfiles,
		Flags:   discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}); err != nil {
		log.Printf("error: upload files channel=%s: %v", channelID, err)
		if _, err2 := discordSend(s, channelID, "Failed to upload files to Discord: "+err.Error()); err2 != nil {
			log.Printf("error: upload fail notice: %v", err2)
		}
		return
	}
	log.Printf("upload: sent %d file(s) to channel=%s", len(dfiles), channelID)
}
