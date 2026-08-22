package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
)

// formFileUploads parses a multipart POST and returns the "images" field files
// (the historical form name; contents are any type, including PDFs).
// Non-multipart requests return (nil, nil) so urlencoded posts keep working.
// MUST be called BEFORE any ctx.PostFormValue in the handler. Note the real
// body bound is requireMember's MaxBytesReader: checkCSRF's FormValue fallback
// usually parses the multipart body before the handler runs, so the wrap below
// only bites on paths that skipped that parse.
func (s *Server) formFileUploads(ctx *hime.Context) ([]bot.WebUpload, error) {
	if ctx == nil || ctx.Request == nil {
		return nil, nil
	}
	ct := ctx.Request.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		return nil, nil
	}
	// 50 MiB files + form overhead.
	ctx.Request.Body = http.MaxBytesReader(ctx.ResponseWriter(), ctx.Request.Body, 52<<20)
	if err := ctx.Request.ParseMultipartForm(32 << 20); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, fmt.Errorf("attachments too large (max 50 MiB total)")
		}
		if strings.Contains(err.Error(), "request body too large") {
			return nil, fmt.Errorf("attachments too large (max 50 MiB total)")
		}
		return nil, fmt.Errorf("parse uploads: %w", err)
	}
	if ctx.Request.MultipartForm == nil {
		return nil, nil
	}
	fhs := ctx.Request.MultipartForm.File["images"]
	if len(fhs) == 0 {
		return nil, nil
	}
	out := make([]bot.WebUpload, 0, len(fhs))
	for _, fh := range fhs {
		if fh == nil {
			continue
		}
		// Browsers send one empty part for an untouched file input.
		if fh.Filename == "" && fh.Size == 0 {
			continue
		}
		out = append(out, bot.WebUpload{
			Filename: fh.Filename,
			Size:     fh.Size,
			Open: func() (io.ReadCloser, error) {
				return fh.Open()
			},
		})
	}
	return out, nil
}
