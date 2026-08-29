package bot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/timeline"
)

const (
	maxUploadFiles = 10
	maxUploadBytes = 25 << 20 // Discord default ~25 MiB
)

// ARTIFACT / DISCORD_UPLOAD block forms:
//
//	ARTIFACT:
//	- path/to/file.apk
//	path/to/report.xlsx
//
//	ARTIFACT: path/to/file.apk
//
// DISCORD_UPLOAD: is accepted as an alias.
var (
	uploadBlockRE = regexp.MustCompile(`(?im)^[ \t]*(?:DISCORD_UPLOAD|ARTIFACT):[ \t]*\n((?:[ \t]*(?:[-*•]\s+)?\S+[ \t]*\n?)*)`)
	uploadLineRE  = regexp.MustCompile(`(?im)^[ \t]*(?:DISCORD_UPLOAD|ARTIFACT):[ \t]+(\S+)[ \t]*$`)
)

// parseUploadPaths extracts file paths from ARTIFACT / DISCORD_UPLOAD markers.
func parseUploadPaths(text string) []string {
	if text == "" {
		return nil
	}
	var raw []string
	for _, m := range uploadBlockRE.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		for line := range strings.SplitSeq(m[1], "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "-")
			line = strings.TrimPrefix(line, "*")
			line = strings.TrimPrefix(line, "•")
			line = strings.TrimSpace(line)
			line = strings.Trim(line, "`\"'")
			if line == "" {
				continue
			}
			if strings.EqualFold(line, "DISCORD_UPLOAD:") || strings.EqualFold(line, "ARTIFACT:") {
				continue
			}
			raw = append(raw, line)
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
		return "", fmt.Errorf("path is outside the worktree")
	}

	// Prefer real path so symlinks cannot escape the worktree.
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// File missing or intermediate link issue — still check existence for clearer error.
		if st, stErr := os.Stat(candidate); stErr != nil {
			return "", fmt.Errorf("file not found")
		} else if st.IsDir() {
			return "", fmt.Errorf("path is a directory, not a file")
		}
		// Stat ok but EvalSymlinks failed (rare); use candidate if still under root.
		return candidate, nil
	}
	real = filepath.Clean(real)
	if !pathInsideRoot(real, root) {
		// Also allow if root itself is a symlink target chain.
		realRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil || !pathInsideRoot(real, filepath.Clean(realRoot)) {
			return "", fmt.Errorf("path escapes the worktree via symlink")
		}
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("file not found")
	}
	if st.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file")
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
			notes = append(notes, skipStoreNote(displayUploadPath(p), err))
			continue
		}
		st, err := os.Stat(abs)
		if err != nil {
			notes = append(notes, skipStoreNote(displayUploadPath(p), err))
			continue
		}
		if st.Size() > maxUploadBytes {
			notes = append(notes, fmt.Sprintf("skip %q: file is %s (max %s)", displayUploadPath(p), formatBytes(st.Size()), formatBytes(maxUploadBytes)))
			continue
		}
		const maxBatch = 100 << 20 // 100 MiB across one upload batch
		if total+st.Size() > maxBatch {
			notes = append(notes, fmt.Sprintf("skip %q: batch size limit reached", displayUploadPath(p)))
			continue
		}
		total += st.Size()
		name := filepath.Base(abs)
		rel := filepath.Base(abs)
		if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = filepath.ToSlash(r)
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

func skipStoreNote(label string, err error) string {
	reason := "could not store"
	if err != nil {
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			msg := err.Error()
			if msg != "" && !strings.ContainsAny(msg, `/\`) {
				reason = msg
			}
		}
	}
	return fmt.Sprintf("skip %q: %s", label, reason)
}

func discordArtifactBatch(files []history.Attachment) (send []history.Attachment, extra []string) {
	if len(files) <= maxUploadFiles {
		return files, nil
	}
	return files[:maxUploadFiles], []string{
		fmt.Sprintf("Discord attached the first %d of %d files; the rest are on the web session.", maxUploadFiles, len(files)),
	}
}

func displayUploadPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return filepath.Base(p)
	}
	return p
}

// artifactPromptLines is the session-file contract appended to every run
// prefix that has a cwd. hasWorktree gates path-based ARTIFACT: instructions.
func artifactPromptLines(hasWorktree bool) []string {
	lines := []string{
		"",
		"Sharing files with this session: files you send are stored on the session and downloadable from the web UI (and attached on Discord when this unit has a thread).",
	}
	if hasWorktree {
		return append(lines,
			"Write the file under THIS worktree, then either call session_send_file (grokwork MCP) or end your reply with:",
			"ARTIFACT:",
			"path/relative/to/worktree/file.xlsx",
			"(one path per line; DISCORD_UPLOAD: is accepted as an alias; max 10 files, 25 MiB each).",
			"Do not list paths outside this worktree — they will be rejected.",
			"You may write new deliverable files (reports, exports, images). Do not edit application source just to produce a file.",
		)
	}
	return append(lines,
		"There is no isolated worktree on this unit. Prefer session_send_file with content (name + encoding=text or base64).",
		"Do not promise Discord attachments when there is no worktree.",
	)
}

// ingestWorktreeArtifacts copies ARTIFACT:/DISCORD_UPLOAD: paths from the
// worktree into the session store. Discord delivery is a separate step.
func (b *Bot) ingestWorktreeArtifacts(threadID, worktreeRoot, text string) []string {
	paths := parseUploadPaths(text)
	if len(paths) == 0 {
		return nil
	}
	files, notes := prepareWorktreeUploads(worktreeRoot, paths)
	for _, n := range notes {
		log.Printf("upload: %s", n)
	}
	for _, f := range files {
		if b.hasRunArtifact(threadID, f.Name, f.RelLabel) {
			continue
		}
		if _, err := b.persistSessionFile(threadID, history.File{
			Path:        f.Path,
			Name:        f.Name,
			ContentType: f.MIME,
			Rel:         f.RelLabel,
		}); err != nil {
			notes = append(notes, skipStoreNote(f.RelLabel, err))
			log.Printf("upload: persist %s: %v", f.RelLabel, err)
		}
	}
	return notes
}

func (b *Bot) persistSessionFile(threadID string, src history.File) (history.Attachment, error) {
	if b == nil || b.history == nil {
		return history.Attachment{}, fmt.Errorf("history unavailable")
	}
	att, err := b.history.PutArtifact(threadID, src)
	if err != nil {
		return history.Attachment{}, err
	}
	b.publishRunArtifact(threadID, att)
	b.appendTimeline(threadID, timeline.KindArtifact, timeline.Artifact{
		Name:        att.Name,
		Rel:         att.Rel,
		ContentType: att.ContentType,
		Size:        att.Size,
	})
	return att, nil
}

// ReceiveSessionFileBytes stores an agent-provided payload as a session file.
// name is a basename; rel is optional worktree-relative display (never stored
// if it looks host-absolute).
func (b *Bot) ReceiveSessionFileBytes(threadID, name, contentType, rel string, content []byte) (history.Attachment, error) {
	if len(content) == 0 {
		return history.Attachment{}, fmt.Errorf("empty file")
	}
	return b.persistSessionFile(threadID, history.File{
		Name:        name,
		ContentType: contentType,
		Rel:         rel,
		Bytes:       content,
	})
}

// ReceiveSessionFileFromWorktree copies a path that must resolve inside the
// unit's worktree into the session store.
func (b *Bot) ReceiveSessionFileFromWorktree(threadID, p string) (history.Attachment, error) {
	root := b.sessionWorktreeRoot(threadID)
	if root == "" {
		return history.Attachment{}, fmt.Errorf("no worktree for this session")
	}
	abs, err := resolveWorktreeUploadPath(root, p)
	if err != nil {
		return history.Attachment{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return history.Attachment{}, fmt.Errorf("file not found")
	}
	if st.Size() > maxUploadBytes {
		return history.Attachment{}, fmt.Errorf("file is %s (max %s)", formatBytes(st.Size()), formatBytes(maxUploadBytes))
	}
	name := filepath.Base(abs)
	rel := name
	if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
		rel = filepath.ToSlash(r)
	}
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	return b.persistSessionFile(threadID, history.File{
		Path:        abs,
		Name:        name,
		ContentType: ctype,
		Rel:         rel,
	})
}

func (b *Bot) sessionWorktreeRoot(threadID string) string {
	if b == nil {
		return ""
	}
	project := ""
	main := ""
	cwd := ""
	if b.sessions != nil {
		if e, ok := b.sessions.Get(threadID); ok {
			project = e.Project
			cwd = e.Cwd
			main = e.MainCwd
		}
	}
	if v, ok := b.states.Load(threadID); ok {
		st := v.(*threadState)
		st.mu.Lock()
		job := st.job
		st.mu.Unlock()
		if job != nil {
			job.mu.Lock()
			if job.cwd != "" {
				cwd = job.cwd
			}
			if project == "" {
				project = job.project
			}
			job.mu.Unlock()
		}
	}
	if main == "" && b.cfg != nil && project != "" {
		if p, ok := b.cfg.ProjectPath(project); ok {
			main = p
		}
	}
	if cwd != "" && cwd != main {
		return cwd
	}
	if b.cfg == nil {
		return ""
	}
	path, onDisk := gitworktree.ResolveSessionWorktreePath(b.cfg.WorktreesRoot(), project, threadID, cwd, main)
	if !onDisk || path == "" || path == main {
		return ""
	}
	return path
}

func (b *Bot) hasRunArtifact(threadID, name, rel string) bool {
	for _, a := range b.runArtifacts(threadID) {
		if a.Name == name || (rel != "" && a.Rel == rel) {
			return true
		}
	}
	return false
}

func (b *Bot) runArtifacts(threadID string) []history.Attachment {
	if b == nil || threadID == "" {
		return nil
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return nil
	}
	st := v.(*threadState)
	st.mu.Lock()
	job := st.job
	st.mu.Unlock()
	if job == nil {
		return nil
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return slices.Clone(job.artifacts)
}

func (b *Bot) publishRunArtifact(threadID string, att history.Attachment) {
	if b == nil || threadID == "" || att.Name == "" {
		return
	}
	v, ok := b.states.Load(threadID)
	if !ok {
		return
	}
	st := v.(*threadState)
	st.mu.Lock()
	job := st.job
	st.mu.Unlock()
	if job == nil {
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	for _, a := range job.artifacts {
		if a.Name == att.Name {
			return
		}
	}
	job.artifacts = append(job.artifacts, att)
}

// discordSendSessionArtifacts attaches persisted session files to the Discord
// thread. Local paths never enter the message — only basenames / rel labels.
func (b *Bot) discordSendSessionArtifacts(s *discordgo.Session, channelID string, files []history.Attachment, notes []string) {
	if s == nil || channelID == "" || b == nil || b.history == nil {
		return
	}
	for _, n := range notes {
		log.Printf("upload: %s", n)
	}
	var extra []string
	files, extra = discordArtifactBatch(files)
	notes = append(notes, extra...)
	if len(files) == 0 {
		if len(notes) == 0 {
			return
		}
		msg := "Could not store requested files (must exist inside this thread's worktree, ≤25 MiB each)."
		msg += "\n" + strings.Join(notes, "\n")
		if _, err := discordSend(s, channelID, msg); err != nil {
			log.Printf("error: upload failure notice: %v", err)
		}
		return
	}

	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	dfiles := make([]*discordgo.File, 0, len(files))
	var names []string
	for _, f := range files {
		r, _, err := b.history.OpenArtifact(channelID, f.Name)
		if err != nil {
			notes = append(notes, fmt.Sprintf("skip %q: open failed", f.Name))
			continue
		}
		closers = append(closers, r)
		ctype := f.ContentType
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		dfiles = append(dfiles, &discordgo.File{
			Name:        f.Name,
			ContentType: ctype,
			Reader:      r,
		})
		label := f.Name
		if f.Rel != "" {
			label = f.Rel
		}
		names = append(names, fmt.Sprintf("`%s` (%s)", label, formatBytes(f.Size)))
	}
	if len(dfiles) == 0 {
		if _, err := discordSend(s, channelID, "Could not open files for upload:\n"+strings.Join(notes, "\n")); err != nil {
			log.Printf("error: upload open notice: %v", err)
		}
		return
	}

	content := "📎 **Files added to this session**\n" + strings.Join(names, "\n")
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
		if _, err2 := discordSend(s, channelID, "Failed to attach files on Discord (they are still downloadable from the web session)."); err2 != nil {
			log.Printf("error: upload fail notice: %v", err2)
		}
		return
	}
	log.Printf("upload: sent %d file(s) to channel=%s", len(dfiles), channelID)
}
