package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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
	Path string
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
			Name:      e.Name,
			IsDir:     e.IsDir,
			Size:      e.Size,
			SizeHuman: formatFileBytes(e.Size),
		}
		if !e.Updated.IsZero() {
			row.Updated = e.Updated
			row.UpdatedText = e.Updated.UTC().Format("2006-01-02 15:04")
		}
		if e.IsDir {
			row.Path = joinFilesPath(subPath, e.Name)
		} else {
			row.Object = joinFilesPath(subPath, e.Name)
		}
		d.FilesRows = append(d.FilesRows, row)
	}
	return s.viewPage(ctx, "files", d)
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
	fh, subPath, overwrite, parseErr := s.parseFileUpload(ctx)
	if parseErr != nil {
		return s.filesRedirect(ctx, project, subPath, "", parseErr)
	}

	st := s.cfg.EffectiveStorage(project)
	if st == nil {
		return s.filesRedirect(ctx, project, subPath, "", fmt.Errorf("file storage is not configured for this project"))
	}

	leaf := filestore.SanitizeFilename(fh.Filename)
	object := joinFilesPath(subPath, leaf)
	if err := filestore.ValidateObjectPath(object); err != nil {
		return s.filesRedirect(ctx, project, subPath, "", err)
	}

	var upErr error
	defer func() {
		s.auditAction(ctx, audit.ActionStorageUpload, upErr, storageAuditDetail(st, project, object, fh.Size, true))
	}()

	tmpDir, err := os.MkdirTemp("", "grokwork-upload-*")
	if err != nil {
		upErr = err
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	defer os.RemoveAll(tmpDir)

	src, err := fh.Open()
	if err != nil {
		upErr = err
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	defer src.Close()

	localPath := filepath.Join(tmpDir, leaf)
	dst, err := os.Create(localPath)
	if err != nil {
		upErr = err
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	// Cap at maxFileUploadBytes even if Content-Length lied.
	written, err := io.Copy(dst, io.LimitReader(src, maxFileUploadBytes+1))
	closeErr := dst.Close()
	if err != nil {
		upErr = err
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	if closeErr != nil {
		upErr = closeErr
		return s.filesRedirect(ctx, project, subPath, "", closeErr)
	}
	if written > maxFileUploadBytes {
		upErr = fmt.Errorf("file exceeds %s limit", formatFileBytes(maxFileUploadBytes))
		return s.filesRedirect(ctx, project, subPath, "", upErr)
	}

	be, err := s.filesBackend(storageTarget(st))
	if err != nil {
		upErr = err
		return s.filesRedirect(ctx, project, subPath, "", err)
	}
	upErr = be.Upload(ctx.Context(), localPath, storageTarget(st), object, overwrite)
	if upErr != nil {
		return s.filesRedirect(ctx, project, subPath, "", upErr)
	}
	return s.filesRedirect(ctx, project, subPath, fmt.Sprintf("Uploaded %s", leaf), nil)
}

// fileUploadHeader is the multipart file field we accept.
type fileUploadHeader struct {
	Filename string
	Size     int64
	Open     func() (io.ReadCloser, error)
}

// parseFileUpload parses the multipart form. MUST run before PostFormValue.
func (s *Server) parseFileUpload(ctx *hime.Context) (fh fileUploadHeader, subPath string, overwrite bool, err error) {
	if ctx == nil || ctx.Request == nil {
		return fh, "", false, fmt.Errorf("missing request")
	}
	// requireMember already capped the body; re-wrap as belt-and-braces for the
	// single-file 50 MiB limit (multipart framing is small on top).
	ctx.Request.Body = http.MaxBytesReader(ctx.ResponseWriter(), ctx.Request.Body, maxFileUploadBytes+2<<20)
	if err := ctx.Request.ParseMultipartForm(32 << 20); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return fh, "", false, fmt.Errorf("file too large (max %s)", formatFileBytes(maxFileUploadBytes))
		}
		if strings.Contains(err.Error(), "request body too large") {
			return fh, "", false, fmt.Errorf("file too large (max %s)", formatFileBytes(maxFileUploadBytes))
		}
		return fh, "", false, fmt.Errorf("parse upload: %w", err)
	}
	// Form values only after ParseMultipartForm.
	subPath = strings.Trim(strings.TrimSpace(ctx.PostFormValue("path")), "/")
	if subPath != "" {
		if err := filestore.ValidateObjectPath(subPath); err != nil {
			return fh, "", false, fmt.Errorf("invalid path: %w", err)
		}
	}
	overwrite = ctx.PostFormValue("overwrite") == "1" ||
		strings.EqualFold(ctx.PostFormValue("overwrite"), "on")

	if ctx.Request.MultipartForm == nil {
		return fh, subPath, overwrite, fmt.Errorf("file is required")
	}
	fhs := ctx.Request.MultipartForm.File["file"]
	if len(fhs) == 0 || fhs[0] == nil {
		return fh, subPath, overwrite, fmt.Errorf("file is required")
	}
	header := fhs[0]
	if header.Filename == "" && header.Size == 0 {
		return fh, subPath, overwrite, fmt.Errorf("file is required")
	}
	if header.Size > maxFileUploadBytes {
		return fh, subPath, overwrite, fmt.Errorf("file exceeds %s limit", formatFileBytes(maxFileUploadBytes))
	}
	return fileUploadHeader{
		Filename: header.Filename,
		Size:     header.Size,
		Open:     func() (io.ReadCloser, error) { return header.Open() },
	}, subPath, overwrite, nil
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

// fileDownload is GET /projects/{project}/files/download?object=.
func (s *Server) fileDownload(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	object := strings.Trim(strings.TrimSpace(ctx.FormValue("object")), "/")
	if object == "" || filestore.ValidateObjectPath(object) != nil {
		return ctx.Status(http.StatusBadRequest).Error("invalid object name")
	}
	st := s.cfg.EffectiveStorage(project)
	if st == nil {
		return ctx.Status(http.StatusNotFound).Error("file storage is not configured")
	}

	be, err := s.filesBackend(storageTarget(st))
	if err != nil {
		return ctx.Status(http.StatusBadGateway).Error(err.Error())
	}
	meta, exists, err := be.Describe(ctx.Context(), storageTarget(st), object)
	if err != nil {
		return ctx.Status(http.StatusBadGateway).Error(err.Error())
	}
	if !exists {
		return ctx.Status(http.StatusNotFound).Error("object not found")
	}
	if meta.Size > maxFileDownloadBytes {
		return ctx.Status(http.StatusRequestEntityTooLarge).Error(
			fmt.Sprintf("object is %s; max download is %s", formatFileBytes(meta.Size), formatFileBytes(maxFileDownloadBytes)))
	}

	tmpDir, err := os.MkdirTemp("", "grokwork-download-*")
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error(err.Error())
	}
	defer os.RemoveAll(tmpDir)

	leaf := filestore.SanitizeFilename(path.Base(object))
	dest := filepath.Join(tmpDir, leaf)
	if err := be.Download(ctx.Context(), storageTarget(st), object, dest); err != nil {
		return ctx.Status(http.StatusBadGateway).Error(err.Error())
	}
	f, err := os.Open(dest)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error(err.Error())
	}
	defer f.Close()

	ctype := strings.TrimSpace(meta.ContentType)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	ctx.ResponseWriter().Header().Set("Content-Type", ctype)
	ctx.ResponseWriter().Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename=%q`, leaf))
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

func joinFilesPath(parts ...string) string {
	var segs []string
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			segs = append(segs, p)
		}
	}
	return path.Join(segs...)
}

func fileBreadcrumbs(subPath string) []fileCrumb {
	subPath = strings.Trim(subPath, "/")
	out := []fileCrumb{{Label: "root", Path: ""}}
	if subPath == "" {
		out[0].Last = true
		return out
	}
	var cur string
	for part := range strings.SplitSeq(subPath, "/") {
		if part == "" {
			continue
		}
		cur = joinFilesPath(cur, part)
		out = append(out, fileCrumb{Label: part, Path: cur})
	}
	out[len(out)-1].Last = true
	return out
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
