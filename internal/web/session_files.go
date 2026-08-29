package web

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/filestore"
	"github.com/acoshift/grokwork/internal/history"
)

func sessionFileURL(threadID string, turn int, name string) string {
	threadID = strings.TrimSpace(threadID)
	name = strings.TrimSpace(name)
	if threadID == "" || name == "" || turn < 1 {
		return ""
	}
	return "/sessions/" + url.PathEscape(threadID) + "/turns/" + strconv.Itoa(turn) + "/files/" + url.PathEscape(name)
}

func sessionRunFileURL(threadID, name string) string {
	threadID = strings.TrimSpace(threadID)
	name = strings.TrimSpace(name)
	if threadID == "" || name == "" {
		return ""
	}
	return "/sessions/" + url.PathEscape(threadID) + "/run/files/" + url.PathEscape(name)
}

func sessionArtifactURL(threadID, name string) string {
	threadID = strings.TrimSpace(threadID)
	name = strings.TrimSpace(name)
	if threadID == "" || name == "" {
		return ""
	}
	return "/sessions/" + url.PathEscape(threadID) + "/artifacts/" + url.PathEscape(name)
}

// sessionTurnFile is GET /sessions/{threadID}/turns/{n}/files/{name}.
func (s *Server) sessionTurnFile(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if err := s.ensureSessionPageAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n < 1 {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	name := strings.TrimSpace(ctx.PathValue("name"))
	if s.history == nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	f, meta, err := s.history.OpenFile(threadID, n, name)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	return serveSessionFile(ctx, f, meta)
}

// sessionRunFile is GET /sessions/{threadID}/run/files/{name}.
func (s *Server) sessionRunFile(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if err := s.ensureSessionPageAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	name := strings.TrimSpace(ctx.PathValue("name"))
	if s.bot == nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	f, meta, err := s.bot.OpenRunAttachment(threadID, name)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	return serveSessionFile(ctx, f, meta)
}

// sessionArtifact is GET /sessions/{threadID}/artifacts/{name}.
func (s *Server) sessionArtifact(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if err := s.ensureSessionPageAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	name := strings.TrimSpace(ctx.PathValue("name"))
	if s.history == nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	f, meta, err := s.history.OpenArtifact(threadID, name)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	return serveSessionFile(ctx, f, meta)
}

func serveSessionFile(ctx *hime.Context, f *os.File, meta history.Attachment) error {
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("read file")
	}
	ctype := mediaTypeOnly(meta.ContentType)
	if ctype == "" {
		ctype = mediaTypeOnly(mime.TypeByExtension(filepath.Ext(meta.Name)))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	unsafe := ctype == "text/html" || ctype == "application/xhtml+xml" || ctype == "image/svg+xml"
	mode := fileServeAttachment
	if !unsafe && filePreviewKind(meta.Name, ctype) != "" {
		mode = fileServeInline
	}
	if unsafe {
		ctype = "application/octet-stream"
	}
	leaf := filestore.SanitizeFilename(meta.Name)
	h := ctx.ResponseWriter().Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", contentDispositionHeader(mode, leaf, meta.Name))
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(ctx.ResponseWriter(), ctx.Request, leaf, fi.ModTime(), f)
	return nil
}

func mediaTypeOnly(ctype string) string {
	ct := strings.ToLower(strings.TrimSpace(ctype))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}
