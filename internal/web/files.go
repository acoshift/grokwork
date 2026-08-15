package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/filestore"
	"github.com/acoshift/grokwork/internal/gcs"
	"github.com/acoshift/grokwork/internal/gdrive"
)

const (
	// maxFileUploadBytes caps a single Files-page upload (multipart field).
	maxFileUploadBytes = 50 << 20
	// maxFileDownloadBytes refuses proxying objects larger than this.
	maxFileDownloadBytes = 100 << 20
	// filesListCap bounds rows shown on one page ("bounded and printed").
	filesListCap = 200
)

// fileRow is one listing row for the Files page.
type fileRow struct {
	Name        string
	IsDir       bool
	Size        int64
	SizeHuman   string
	Updated     time.Time
	UpdatedText string
	// Object is the full object path relative to the storage prefix (for
	// download/delete). Empty for directories.
	Object string
	// Path is the ?path= value to enter a directory (relative to prefix).
	Path         string
	ContentType  string
	PreviewKind  string // "", "image", "pdf"
	Downloadable bool
	NativeGoogle bool
	// Icon picks the row glyph (files.tmpl "fileIcon"): folder, image, pdf,
	// gdoc, gsheet, gslides, text, code, archive, video, audio, file.
	Icon string
	// KindLabel is the short human type shown in the viewer meta line
	// ("PNG image", "PDF", "Google Sheet").
	KindLabel string
}

// filesPreview is the in-app lightbox opened from ?preview= on the Files page.
type filesPreview struct {
	Name     string
	Kind     string // image | pdf
	Src      string // byte-stream URL loaded inside the dialog
	Download string
	// Meta is the viewer's one-line description ("PDF · 4.0 KiB · 2026-08-01
	// 10:00"); empty when the object was not in the listing.
	Meta string
}

// fileCrumb is one breadcrumb segment on the Files page.
type fileCrumb struct {
	Label string
	Path  string // empty for root
	Last  bool
}

// gcsRun returns the injected runner, or nil so gcs ops use the default.
func (s *Server) gcsRun() gcs.Runner {
	if s != nil && s.gcsRunner != nil {
		return s.gcsRunner
	}
	return nil
}

// driveTokenSource returns the injected auth, or a JWT bearer for the key path.
func (s *Server) driveTokenSource(creds string) gdrive.TokenSource {
	if s != nil && s.driveAuth != nil {
		return s.driveAuth
	}
	var httpClient *http.Client
	if s != nil {
		httpClient = s.driveHTTP
	}
	return &gdrive.JWTBearer{CredentialsFile: creds, HTTP: httpClient}
}

// filesBackend returns the storage backend for the resolved target.
func (s *Server) filesBackend(t filestore.Target) (filestore.Backend, error) {
	if s != nil && s.filesBackendFn != nil {
		return s.filesBackendFn(t)
	}
	backend := strings.TrimSpace(strings.ToLower(t.Backend))
	switch backend {
	case "", filestore.BackendGCS:
		return filestore.GCS{Run: s.gcsRun()}, nil
	case filestore.BackendGDrive:
		return filestore.GDrive{Client: &gdrive.Client{
			Auth: s.driveTokenSource(t.CredentialsFile),
			HTTP: s.driveHTTP,
		}}, nil
	default:
		return nil, fmt.Errorf("unknown storage backend %q", t.Backend)
	}
}

// storageTarget maps config storage onto the filestore boundary type.
func storageTarget(st *config.StorageConfig) filestore.Target {
	if st == nil {
		return filestore.Target{}
	}
	backend := strings.TrimSpace(strings.ToLower(st.Backend))
	if backend == "" {
		if strings.TrimSpace(st.DriveFolderID) != "" {
			backend = filestore.BackendGDrive
		} else {
			backend = filestore.BackendGCS
		}
	}
	return filestore.Target{
		Backend:          backend,
		Bucket:           st.GCSBucket,
		Prefix:           st.Prefix,
		FolderID:         st.DriveFolderID,
		IsolationSegment: st.IsolationSegment,
		CredentialsFile:  st.CredentialsFile,
	}
}

// storageAuditDetail builds upload/delete audit fields (never credentials path).
func storageAuditDetail(st *config.StorageConfig, project, object string, size int64, withSize bool) map[string]any {
	detail := map[string]any{
		"project": project,
		"object":  object,
	}
	if withSize {
		detail["size"] = size
	}
	if st == nil {
		return detail
	}
	detail["backend"] = st.Backend
	switch strings.TrimSpace(strings.ToLower(st.Backend)) {
	case config.StorageBackendGDrive:
		detail["folderId"] = st.DriveFolderID
		if st.IsolationSegment != "" {
			detail["isolation"] = st.IsolationSegment
		}
	default:
		detail["bucket"] = st.GCSBucket
	}
	return detail
}

// filesPage is GET /projects/{project}/files[?path=].
func (s *Server) filesPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}

	d := s.basePage(ctx)
	d.Title = project + " · Files"
	d.IsFiles = true
	d.Project = project
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	// Fill affordances before every early return so the page renders its own
	// chrome when storage is unset, the backend fails, or the listing fails.
	d.CanStorageWrite = s.cfg.FeatureStorage() &&
		s.cfg.ResolveCapabilities(project, d.UserID).CanStorageWrite()
	d.StorageFeatureOn = s.cfg.FeatureStorage()

	// I/O uses effective target; chrome flags need raw + global.
	raw := s.cfg.ProjectStorage(project)
	global := s.cfg.GlobalStorage()
	eff := s.cfg.EffectiveStorage(project)
	d.StorageDisabled = raw != nil && raw.Disabled
	d.StorageInherited = raw == nil && global != nil && !d.StorageDisabled
	d.StorageNotConfigured = eff == nil && !d.StorageDisabled
	if eff == nil {
		return s.viewPage(ctx, "files", d)
	}
	d.StorageBackend = eff.Backend
	d.StorageBucket = eff.GCSBucket
	d.StoragePrefix = eff.Prefix
	d.StorageDriveFolderID = eff.DriveFolderID
	d.StorageIsolation = eff.IsolationSegment

	subPath := strings.TrimSpace(ctx.FormValue("path"))
	subPath = strings.Trim(subPath, "/")
	if subPath != "" {
		if err := filestore.ValidateObjectPath(subPath); err != nil {
			// Invalid path → treat as root with an inline error; never echo into a URL.
			if d.Error == "" {
				d.Error = "invalid path: " + err.Error()
			}
			subPath = ""
		}
	}
	d.FilesPath = subPath
	d.FilesCrumbs = fileBreadcrumbs(subPath)

	be, err := s.filesBackend(storageTarget(eff))
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "files", d)
	}
	entries, err := be.List(ctx.Context(), storageTarget(eff), subPath)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "files", d)
	}
	d.FilesTotal = len(entries)
	if len(entries) > filesListCap {
		entries = entries[:filesListCap]
		d.FilesClipped = true
	}
	d.FilesRows = make([]fileRow, 0, len(entries))
	for _, e := range entries {
		row := fileRow{
			Name:         e.Name,
			IsDir:        e.IsDir,
			Size:         e.Size,
			SizeHuman:    formatFileBytes(e.Size),
			ContentType:  e.ContentType,
			NativeGoogle: isGoogleNativeMIME(e.ContentType),
		}
		if !e.Updated.IsZero() {
			row.Updated = e.Updated
			row.UpdatedText = e.Updated.UTC().Format("2006-01-02 15:04")
		}
		if e.IsDir {
			row.Path = filestore.AppendName(subPath, e.Name)
			d.FilesDirCount++
		} else {
			row.Object = filestore.AppendName(subPath, e.Name)
			row.Downloadable = !row.NativeGoogle
			if row.Downloadable {
				row.PreviewKind = filePreviewKind(e.Name, e.ContentType)
			}
			d.FilesFileCount++
			d.FilesBytes += e.Size
		}
		row.Icon, row.KindLabel = fileIconKind(e.Name, e.ContentType, e.IsDir)
		d.FilesRows = append(d.FilesRows, row)
	}
	d.FilesBytesHuman = formatFileBytes(d.FilesBytes)
	d.FilesPreview = filesPreviewFromQuery(project, ctx.FormValue("preview"), d.FilesRows)
	return s.viewPage(ctx, "files", d)
}

func filesPreviewFromQuery(project, raw string, rows []fileRow) *filesPreview {
	object := strings.Trim(strings.TrimSpace(raw), "/")
	if object == "" || filestore.ValidateObjectPath(object) != nil {
		return nil
	}
	kind, name, meta := "", "", ""
	if segs, err := filestore.SplitPath(object); err == nil && len(segs) > 0 {
		name = segs[len(segs)-1]
	}
	for _, row := range rows {
		if row.Object == object {
			kind, name, meta = row.PreviewKind, row.Name, row.MetaLine()
			break
		}
	}
	if kind == "" {
		kind = filePreviewKind(name, "")
	}
	if kind == "" {
		return nil
	}
	q := url.QueryEscape(object)
	return &filesPreview{
		Name:     name,
		Kind:     kind,
		Src:      "/projects/" + url.PathEscape(project) + "/files/preview?object=" + q,
		Download: "/projects/" + url.PathEscape(project) + "/files/download?object=" + q,
		Meta:     meta,
	}
}

// MetaLine is the "PDF · 4.0 KiB · 2026-08-01 10:00" strip under a file's
// name — the row's mobile subtitle and the viewer's header both print it, so
// the two cannot phrase the same file differently.
func (r fileRow) MetaLine() string {
	var parts []string
	if r.KindLabel != "" && !r.IsDir {
		parts = append(parts, r.KindLabel)
	}
	if !r.IsDir {
		parts = append(parts, r.SizeHuman)
	}
	if r.UpdatedText != "" {
		parts = append(parts, r.UpdatedText)
	}
	return strings.Join(parts, " · ")
}

// fileIconKind maps a listing entry onto a row glyph and a short type label.
// Icons come from a closed set the template knows how to draw; anything
// unrecognised is a plain "file", never an empty glyph.
func fileIconKind(name, ctype string, isDir bool) (icon, label string) {
	if isDir {
		return "folder", "Folder"
	}
	ct := strings.ToLower(strings.TrimSpace(ctype))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/vnd.google-apps.document":
		return "gdoc", "Google Doc"
	case "application/vnd.google-apps.spreadsheet":
		return "gsheet", "Google Sheet"
	case "application/vnd.google-apps.presentation":
		return "gslides", "Google Slides"
	}
	if isGoogleNativeMIME(ct) {
		return "gdoc", "Google file"
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	upper := strings.ToUpper(ext)
	switch {
	case ct == "application/pdf" || ext == "pdf":
		return "pdf", "PDF"
	case strings.HasPrefix(ct, "image/"), ext == "png", ext == "jpg", ext == "jpeg", ext == "gif", ext == "webp", ext == "svg", ext == "heic", ext == "bmp", ext == "tiff", ext == "tif":
		if upper == "" {
			return "image", "Image"
		}
		return "image", upper + " image"
	case strings.HasPrefix(ct, "video/"), ext == "mp4", ext == "mov", ext == "webm", ext == "mkv":
		return "video", "Video"
	case strings.HasPrefix(ct, "audio/"), ext == "mp3", ext == "wav", ext == "m4a", ext == "ogg", ext == "flac":
		return "audio", "Audio"
	case ext == "zip", ext == "gz", ext == "tgz", ext == "tar", ext == "7z", ext == "rar", ext == "bz2", ext == "xz",
		ct == "application/zip", ct == "application/gzip", ct == "application/x-tar":
		return "archive", "Archive"
	case ext == "go", ext == "js", ext == "ts", ext == "tsx", ext == "jsx", ext == "py", ext == "rb", ext == "rs",
		ext == "java", ext == "kt", ext == "swift", ext == "c", ext == "h", ext == "cpp", ext == "cs", ext == "sh",
		ext == "sql", ext == "html", ext == "css", ext == "json", ext == "yaml", ext == "yml", ext == "toml", ext == "xml":
		return "code", upper
	case strings.HasPrefix(ct, "text/"), ext == "txt", ext == "md", ext == "csv", ext == "log", ext == "rtf":
		if upper == "" {
			return "text", "Text"
		}
		return "text", upper
	case ext == "doc", ext == "docx", ext == "odt", ext == "pages":
		return "text", upper
	case ext == "xls", ext == "xlsx", ext == "ods", ext == "numbers":
		return "gsheet", upper
	case ext == "ppt", ext == "pptx", ext == "odp", ext == "key":
		return "gslides", upper
	}
	if upper != "" && len(upper) <= 6 {
		return "file", upper
	}
	return "file", "File"
}

// postFileUpload is POST /projects/{project}/files/upload.
func (s *Server) postFileUpload(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).CanStorageWrite() {
		// Denials are events too — a refused write is what an operator goes
		// looking for in the audit log.
		denied := fmt.Errorf("not allowed to upload files for this project")
		s.auditAction(ctx, audit.ActionStorageUpload, denied, map[string]any{"project": project})
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	// Parse multipart BEFORE any PostFormValue (uploads.go ordering contract).
	fhs, subPath, overwrite, parseErr := s.parseFileUpload(ctx)
	if parseErr != nil {
		return s.filesRedirect(ctx, project, subPath, "", parseErr)
	}

	st := s.cfg.EffectiveStorage(project)
	if st == nil {
		return s.filesRedirect(ctx, project, subPath, "", fmt.Errorf("file storage is not configured for this project"))
	}

	be, err := s.filesBackend(storageTarget(st))
	if err != nil {
		return s.filesRedirect(ctx, project, subPath, "", err)
	}

	tmpDir, err := os.MkdirTemp("", "grokwork-upload-*")
	if err != nil {
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	defer os.RemoveAll(tmpDir)

	var uploaded []string
	for i, fh := range fhs {
		leaf := filestore.SanitizeFilename(fh.Filename)
		object := filestore.AppendName(subPath, leaf)
		if err := filestore.ValidateObjectPath(object); err != nil {
			return s.uploadPartialRedirect(ctx, project, subPath, uploaded, leaf, err)
		}

		var upErr error
		func() {
			defer func() {
				s.auditAction(ctx, audit.ActionStorageUpload, upErr, storageAuditDetail(st, project, object, fh.Size, true))
			}()

			src, err := fh.Open()
			if err != nil {
				upErr = err
				return
			}
			defer src.Close()

			localPath := filepath.Join(tmpDir, fmt.Sprintf("%d-%s", i, leaf))
			dst, err := os.Create(localPath)
			if err != nil {
				upErr = err
				return
			}
			written, err := io.Copy(dst, io.LimitReader(src, maxFileUploadBytes+1))
			closeErr := dst.Close()
			if err != nil {
				upErr = err
				return
			}
			if closeErr != nil {
				upErr = closeErr
				return
			}
			if written > maxFileUploadBytes {
				upErr = fmt.Errorf("file exceeds %s limit", formatFileBytes(maxFileUploadBytes))
				return
			}
			upErr = be.Upload(ctx.Context(), localPath, storageTarget(st), object, overwrite)
		}()
		if upErr != nil {
			return s.uploadPartialRedirect(ctx, project, subPath, uploaded, leaf, upErr)
		}
		uploaded = append(uploaded, leaf)
	}
	return s.filesRedirect(ctx, project, subPath, fmt.Sprintf("Uploaded %s", strings.Join(uploaded, ", ")), nil)
}

func (s *Server) uploadPartialRedirect(ctx *hime.Context, project, subPath string, uploaded []string, failed string, err error) error {
	if len(uploaded) == 0 {
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	return s.filesRedirect(ctx, project, subPath, "",
		fmt.Errorf("uploaded %s; %s failed: %w", strings.Join(uploaded, ", "), failed, err))
}

// fileUploadHeader is the multipart file field we accept.
type fileUploadHeader struct {
	Filename string
	Size     int64
	Open     func() (io.ReadCloser, error)
}

// parseFileUpload parses the multipart form. MUST run before PostFormValue.
// Body size is already capped at memberMutationBodyLimit by requireMember
// (before CSRF parses the multipart). Per-file 50 MiB is checked here and
// again with LimitReader when staging.
func (s *Server) parseFileUpload(ctx *hime.Context) (fhs []fileUploadHeader, subPath string, overwrite bool, err error) {
	if ctx == nil || ctx.Request == nil {
		return nil, "", false, fmt.Errorf("missing request")
	}
	if err := ctx.Request.ParseMultipartForm(32 << 20); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, "", false, fmt.Errorf("request exceeds %s limit", formatFileBytes(memberMutationBodyLimit))
		}
		if strings.Contains(err.Error(), "request body too large") {
			return nil, "", false, fmt.Errorf("request exceeds %s limit", formatFileBytes(memberMutationBodyLimit))
		}
		return nil, "", false, fmt.Errorf("parse upload: %w", err)
	}
	subPath = strings.Trim(strings.TrimSpace(ctx.PostFormValue("path")), "/")
	if subPath != "" {
		if err := filestore.ValidateObjectPath(subPath); err != nil {
			return nil, "", false, fmt.Errorf("invalid path: %w", err)
		}
	}
	overwrite = ctx.PostFormValue("overwrite") == "1" ||
		strings.EqualFold(ctx.PostFormValue("overwrite"), "on")

	if ctx.Request.MultipartForm == nil {
		return nil, subPath, overwrite, fmt.Errorf("file is required")
	}
	headers := ctx.Request.MultipartForm.File["file"]
	out := make([]fileUploadHeader, 0, len(headers))
	for _, header := range headers {
		if header == nil {
			continue
		}
		if header.Filename == "" && header.Size == 0 {
			continue
		}
		if header.Size > maxFileUploadBytes {
			return nil, subPath, overwrite, fmt.Errorf("file exceeds %s limit", formatFileBytes(maxFileUploadBytes))
		}
		h := header
		out = append(out, fileUploadHeader{
			Filename: h.Filename,
			Size:     h.Size,
			Open:     func() (io.ReadCloser, error) { return h.Open() },
		})
	}
	if len(out) == 0 {
		return nil, subPath, overwrite, fmt.Errorf("file is required")
	}
	return out, subPath, overwrite, nil
}

// postFileDelete is POST /projects/{project}/files/delete.
func (s *Server) postFileDelete(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).CanStorageWrite() {
		denied := fmt.Errorf("not allowed to delete files for this project")
		s.auditAction(ctx, audit.ActionStorageDelete, denied, map[string]any{"project": project})
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	object := strings.Trim(strings.TrimSpace(ctx.PostFormValue("object")), "/")
	subPath := strings.Trim(strings.TrimSpace(ctx.PostFormValue("path")), "/")
	if subPath != "" {
		if err := filestore.ValidateObjectPath(subPath); err != nil {
			subPath = ""
		}
	}

	st := s.cfg.EffectiveStorage(project)
	if st == nil {
		return s.filesRedirect(ctx, project, subPath, "", fmt.Errorf("file storage is not configured for this project"))
	}
	if err := filestore.ValidateObjectPath(object); err != nil || object == "" {
		err := fmt.Errorf("invalid object name")
		if object != "" {
			if e := filestore.ValidateObjectPath(object); e != nil {
				err = e
			}
		}
		s.auditAction(ctx, audit.ActionStorageDelete, err, storageAuditDetail(st, project, object, 0, false))
		return s.filesRedirect(ctx, project, subPath, "", err)
	}

	be, err := s.filesBackend(storageTarget(st))
	if err != nil {
		s.auditAction(ctx, audit.ActionStorageDelete, err, storageAuditDetail(st, project, object, 0, false))
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	delErr := be.Delete(ctx.Context(), storageTarget(st), object)
	s.auditAction(ctx, audit.ActionStorageDelete, delErr, storageAuditDetail(st, project, object, 0, false))
	if delErr != nil {
		return s.filesRedirect(ctx, project, subPath, "", delErr)
	}
	return s.filesRedirect(ctx, project, subPath, fmt.Sprintf("Deleted %s", path.Base(object)), nil)
}

type fileServeMode int

const (
	fileServeAttachment fileServeMode = iota
	fileServeInline
)

// fileDownload is GET /projects/{project}/files/download?object=.
func (s *Server) fileDownload(ctx *hime.Context) error {
	return s.serveStoredFile(ctx, fileServeAttachment)
}

// filePreview is GET /projects/{project}/files/preview?object=.
func (s *Server) filePreview(ctx *hime.Context) error {
	return s.serveStoredFile(ctx, fileServeInline)
}

func (s *Server) serveStoredFile(ctx *hime.Context, mode fileServeMode) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	object := strings.Trim(strings.TrimSpace(ctx.FormValue("object")), "/")
	fail := func(status int, err error) error {
		if mode == fileServeAttachment {
			parent := path.Dir(object)
			if parent == "." {
				parent = ""
			}
			return s.filesRedirect(ctx, project, parent, "", err)
		}
		return ctx.Status(status).Error(err.Error())
	}
	if object == "" || filestore.ValidateObjectPath(object) != nil {
		err := fmt.Errorf("invalid object name")
		if object != "" {
			if e := filestore.ValidateObjectPath(object); e != nil {
				err = e
			}
		}
		return fail(http.StatusBadRequest, err)
	}
	st := s.cfg.EffectiveStorage(project)
	if st == nil {
		return fail(http.StatusNotFound, fmt.Errorf("file storage is not configured"))
	}

	be, err := s.filesBackend(storageTarget(st))
	if err != nil {
		return fail(http.StatusBadGateway, err)
	}
	tgt := storageTarget(st)
	meta, exists, err := be.Describe(ctx.Context(), tgt, object)
	if err != nil {
		return fail(http.StatusBadGateway, err)
	}
	if !exists {
		return fail(http.StatusNotFound, fmt.Errorf("object not found"))
	}
	if isGoogleNativeMIME(meta.ContentType) {
		action := "downloaded"
		if mode == fileServeInline {
			action = "previewed"
		}
		return fail(http.StatusUnsupportedMediaType,
			fmt.Errorf("Google Docs/Sheets cannot be %s here; export as PDF first", action))
	}
	if meta.Size > maxFileDownloadBytes {
		return fail(http.StatusRequestEntityTooLarge,
			fmt.Errorf("object is %s; max download is %s", formatFileBytes(meta.Size), formatFileBytes(maxFileDownloadBytes)))
	}
	if mode == fileServeInline && filePreviewKind(meta.Name, meta.ContentType) == "" &&
		filePreviewKind(path.Base(object), meta.ContentType) == "" {
		return fail(http.StatusUnsupportedMediaType, fmt.Errorf("this file type cannot be previewed"))
	}

	tmpDir, err := os.MkdirTemp("", "grokwork-download-*")
	if err != nil {
		return fail(http.StatusInternalServerError, err)
	}
	defer os.RemoveAll(tmpDir)

	leaf := filestore.SanitizeFilename(path.Base(object))
	dest := filepath.Join(tmpDir, leaf)
	if err := be.Download(ctx.Context(), tgt, object, dest); err != nil {
		return fail(http.StatusBadGateway, err)
	}
	f, err := os.Open(dest)
	if err != nil {
		return fail(http.StatusInternalServerError, err)
	}
	defer f.Close()

	ctype := strings.TrimSpace(meta.ContentType)
	if ctype == "" {
		if t := mime.TypeByExtension(path.Ext(leaf)); t != "" {
			ctype = t
		} else {
			ctype = "application/octet-stream"
		}
	}
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = strings.TrimSpace(ctype[:i])
	}
	original := path.Base(object)
	if meta.Name != "" {
		original = meta.Name
	}
	h := ctx.ResponseWriter().Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", contentDispositionHeader(mode, leaf, original))
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(ctx.ResponseWriter(), ctx.Request, leaf, meta.Updated, f)
	return nil
}

// setProjectStorage is POST /config/projects/storage (Integrations tab).
// action=save|clear|disable — empty identity on save is rejected (never clear).
func (s *Server) setProjectStorage(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	action := strings.ToLower(strings.TrimSpace(ctx.PostFormValue("action")))
	if action == "" {
		action = "save"
	}
	backend := strings.TrimSpace(ctx.PostFormValue("backend"))
	bucket := strings.TrimSpace(ctx.PostFormValue("gcsBucket"))
	prefix := strings.TrimSpace(ctx.PostFormValue("prefix"))
	folderID := strings.TrimSpace(ctx.PostFormValue("driveFolderId"))
	credentialsFile := strings.TrimSpace(ctx.PostFormValue("credentialsFile"))

	var err error
	var msg string
	switch action {
	case "clear":
		err = s.cfg.ClearProjectStorage(name)
		if s.cfg.GlobalStorage() != nil {
			msg = fmt.Sprintf("Cleared storage override for project %q (using global default)", name)
		} else {
			msg = fmt.Sprintf("Unlinked file storage for project %q", name)
		}
	case "disable":
		err = s.cfg.SetProjectStorageDisabled(name)
		msg = fmt.Sprintf("Disabled file storage for project %q", name)
	case "save":
		err = s.cfg.SetProjectStorage(name, config.StorageInput{
			Backend:         backend,
			GCSBucket:       bucket,
			Prefix:          prefix,
			DriveFolderID:   folderID,
			CredentialsFile: credentialsFile,
		})
		msg = fmt.Sprintf("Updated file storage for project %q", name)
		// Non-blocking isolation warnings when override reuses the global root.
		if err == nil {
			if st := s.cfg.ProjectStorage(name); st != nil {
				if g := s.cfg.GlobalStorage(); g != nil {
					switch st.Backend {
					case config.StorageBackendGCS:
						if st.GCSBucket == g.GCSBucket && g.GCSBucket != "" {
							op := strings.TrimSpace(st.Prefix)
							gp := strings.TrimSpace(g.Prefix)
							if op != "" && op != gp {
								if want, jerr := config.JoinStoragePrefix(g.Prefix, name); jerr == nil {
									if st.Prefix != want && !strings.HasPrefix(st.Prefix, want+"/") {
										msg += ". Warning: this project uses the same bucket as the global default without the isolated prefix " + want + ". Other projects may see the same objects."
									}
								}
							}
						}
					}
				}
			}
		}
	default:
		err = fmt.Errorf("unknown storage action %q", action)
	}

	// The detail carries whether a key file is set, never its path (no local
	// paths in audit) and never its contents.
	detail := map[string]any{
		"name":               name,
		"backend":            backend,
		"bucket":             bucket,
		"prefix":             prefix,
		"folderId":           folderID,
		"credentialsFileSet": credentialsFile != "",
		"action":             action,
	}
	s.auditAction(ctx, audit.ActionConfigSetProjectStorage, err, detail)
	return s.projectConfigTabRedirect(ctx, name, "integrations", msg, err)
}

// storageConfigPage is GET /config/storage.
func (s *Server) storageConfigPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "File storage · Config"
	d.IsConfig = true
	d.Config = s.cfg.Snapshot()
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	d.StorageInheritCount = s.cfg.CountInheritingStorageProjects()
	return s.viewPage(ctx, "config_storage", d)
}

// setGlobalStorage is POST /config/storage.
func (s *Server) setGlobalStorage(ctx *hime.Context) error {
	backend := strings.TrimSpace(ctx.PostFormValue("backend"))
	bucket := strings.TrimSpace(ctx.PostFormValue("gcsBucket"))
	prefix := strings.TrimSpace(ctx.PostFormValue("prefix"))
	folderID := strings.TrimSpace(ctx.PostFormValue("driveFolderId"))
	credentialsFile := strings.TrimSpace(ctx.PostFormValue("credentialsFile"))
	inheritN := s.cfg.CountInheritingStorageProjects()
	err := s.cfg.SetGlobalStorage(config.StorageInput{
		Backend:         backend,
		GCSBucket:       bucket,
		Prefix:          prefix,
		DriveFolderID:   folderID,
		CredentialsFile: credentialsFile,
	})
	// K17: cleared ⇔ identity-based (GlobalStorage nil after successful save).
	cleared := err == nil && s.cfg.GlobalStorage() == nil
	s.auditAction(ctx, audit.ActionConfigSetGlobalStorage, err, map[string]any{
		"backend":             backend,
		"bucket":              bucket,
		"prefix":              prefix,
		"folderId":            folderID,
		"credentialsFileSet":  credentialsFile != "",
		"cleared":             cleared,
		"inheritProjectCount": inheritN,
	})
	msg := "Updated global file storage default"
	if cleared {
		msg = "Cleared global file storage default"
	}
	return s.configPageRedirect(ctx, "config.storage", msg, err)
}

// filesRedirect returns to the Files page preserving ?path= plus ok=/err=.
func (s *Server) filesRedirect(ctx *hime.Context, project, subPath, okMsg string, err error) error {
	project = strings.TrimSpace(project)
	u := "/projects/" + url.PathEscape(project) + "/files"
	q := url.Values{}
	if p := strings.Trim(subPath, "/"); p != "" {
		q.Set("path", p)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func fileBreadcrumbs(subPath string) []fileCrumb {
	out := []fileCrumb{{Label: "Files", Path: ""}}
	segs, err := filestore.SplitPath(subPath)
	if err != nil || len(segs) == 0 {
		out[0].Last = true
		return out
	}
	var names []string
	for _, name := range segs {
		names = append(names, name)
		out = append(out, fileCrumb{Label: name, Path: filestore.JoinNames(names...)})
	}
	out[len(out)-1].Last = true
	return out
}

const googleAppsFolderMIME = "application/vnd.google-apps.folder"

func isGoogleNativeMIME(m string) bool {
	m = strings.TrimSpace(strings.ToLower(m))
	return strings.HasPrefix(m, "application/vnd.google-apps.") && m != googleAppsFolderMIME
}

func filePreviewKind(name, ctype string) string {
	if isGoogleNativeMIME(ctype) {
		return ""
	}
	ct := strings.ToLower(strings.TrimSpace(ctype))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "text/html" || ct == "application/xhtml+xml" || ct == "image/svg+xml" {
		return ""
	}
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "image"
	case "application/pdf":
		return "pdf"
	}
	if ct == "" || ct == "application/octet-stream" {
		switch strings.ToLower(path.Ext(name)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			return "image"
		case ".pdf":
			return "pdf"
		}
	}
	return ""
}

func contentDispositionHeader(mode fileServeMode, sanitized, original string) string {
	disp := "attachment"
	if mode == fileServeInline {
		disp = "inline"
	}
	var b strings.Builder
	b.WriteString(disp)
	b.WriteString("; filename=")
	b.WriteString(quotedASCIIFilename(sanitized))
	leaf := strings.TrimSpace(original)
	if leaf == "" {
		leaf = sanitized
	}
	b.WriteString("; filename*=UTF-8''")
	b.WriteString(rfc5987Attr(leaf))
	return b.String()
}

func quotedASCIIFilename(name string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteByte('"')
	return b.String()
}

func rfc5987Attr(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_' || r == '~' {
			b.WriteRune(r)
			continue
		}
		for _, by := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", by)
		}
	}
	return b.String()
}

// formatFileBytes is a small local helper (web must not import internal/bot).
func formatFileBytes(n int64) string {
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
