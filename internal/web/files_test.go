package web

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/filestore"
)

// storageServer is the auth-on fixture with the storage feature enabled and a
// bucket linked (service-account key set, so env assertions exercise the
// credentials path), plus a scripted gcs runner. Calls are recorded so tests
// can assert exactly what reached the gcloud boundary.
func storageServer(t *testing.T) (*Server, *config.Config, *[][]string) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.Storage = true
	if err := cfg.SetProjectStorageGCS("proj", "test-bucket", "pre", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	srv.gcsRunner = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if name != "gcloud" {
			t.Errorf("runner name = %q, want gcloud", name)
		}
		// The configured key must reach every gcloud invocation as env.
		if !slices.Contains(env, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=/etc/keys/svc.json") {
			t.Errorf("env = %v, missing credential override", env)
		}
		calls = append(calls, args)
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "storage ls --json"):
			return []byte(`[
				{"url":"gs://test-bucket/pre/report.pdf","type":"cloud_object","metadata":{"name":"pre/report.pdf","size":"2048","updated":"2026-08-01T10:00:00Z","contentType":"application/pdf"}},
				{"url":"gs://test-bucket/pre/sub/","type":"prefix"}
			]`), nil
		case strings.HasPrefix(joined, "storage objects describe"):
			return nil, fmt.Errorf("gcloud storage objects describe: not found: 404")
		case strings.HasPrefix(joined, "storage cp"), strings.HasPrefix(joined, "storage rm"):
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected gcloud args: %s", joined)
	}
	return srv, cfg, &calls
}

func postFilesMultipart(t *testing.T, srv *Server, path, sid, csrf string, fields map[string]string, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestFilesPageListsBucket(t *testing.T) {
	srv, _, calls := storageServer(t)
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, `id="page-project-files"`) {
		t.Fatal("page marker missing")
	}
	for _, want := range []string{"report.pdf", "2.0 KiB", "sub/", "test-bucket"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	// Admin (unmapped → builder) may write: upload form renders.
	if !strings.Contains(body, `id="files-upload"`) {
		t.Fatal("upload form missing for builder-class viewer")
	}
	if len(*calls) != 1 || !strings.Contains(strings.Join((*calls)[0], " "), "gs://test-bucket/pre/") {
		t.Fatalf("ls call = %v", *calls)
	}
}

func TestFilesPageRendersInlineErrorOnRunnerFailure(t *testing.T) {
	srv, _, _ := storageServer(t)
	srv.gcsRunner = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gcloud storage ls: You do not currently have an active account selected")
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, `id="page-project-files"`) {
		t.Fatal("page must render its own chrome on a listing failure")
	}
	if !strings.Contains(body, "active account") {
		t.Fatal("listing error not shown inline")
	}
}

func TestFilesPageInvalidPathFallsBackToRoot(t *testing.T) {
	srv, _, calls := storageServer(t)
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files?path=..%2Fother", sid)
	if !strings.Contains(body, "invalid path") {
		t.Fatal("invalid ?path= not reported inline")
	}
	// The listing must have been for the root, never the traversal value.
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), "..") {
			t.Fatalf("traversal reached the runner: %v", c)
		}
	}
}

func TestFileBreadcrumbs(t *testing.T) {
	got := fileBreadcrumbs("Docs for Customer/CR AMB")
	want := []fileCrumb{
		{Label: "Files", Path: ""},
		{Label: "Docs for Customer", Path: "Docs for Customer"},
		{Label: "CR AMB", Path: "Docs for Customer/CR AMB", Last: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	root := fileBreadcrumbs("")
	if !slices.Equal(root, []fileCrumb{{Label: "Files", Path: "", Last: true}}) {
		t.Fatalf("root = %#v", root)
	}
}

func TestFilesPageBreadcrumbIsInlineTrail(t *testing.T) {
	srv, _, _ := storageServer(t)
	sid, _ := adminLogin(t, srv)
	root := getAuthed(t, srv, "/projects/proj/files", sid)
	if i := strings.Index(root, `id="page-project-files"`); i >= 0 {
		root = root[i:]
	}
	if strings.Contains(root, `class="pr-crumb"`) {
		t.Fatal("root listing must not render a one-segment Files crumb")
	}
	body := getAuthed(t, srv, "/projects/proj/files?path=Docs%20for%20Customer/CR%20AMB", sid)
	// The sidebar owns the element selector `nav { flex-direction: column }`.
	// A files crumb that is itself a <nav> stacks each segment on its own line.
	if i := strings.Index(body, `id="page-project-files"`); i >= 0 {
		body = body[i:]
	}
	if strings.Contains(body, "<nav") {
		t.Fatal("files crumb must not use <nav>; sidebar nav rules stack it vertically")
	}
	if !strings.Contains(body, `class="pr-crumb"`) {
		t.Fatal("files crumb must use the shared inline pr-crumb trail")
	}
	for _, want := range []string{
		`class="pr-crumb"`,
		`aria-label="Current folder"`,
		`>Files</a>`,
		// urlquery emits + for space; html/template then writes &#43; in href.
		`href="/projects/proj/files?path=Docs&#43;for&#43;Customer"`,
		`>Docs for Customer</a>`,
		`>CR AMB</span>`,
		`<span class="crumb-sep">/</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestFilesUploadHappyPath(t *testing.T) {
	srv, _, calls := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFilesMultipart(t, srv, "/projects/proj/files/upload", sid, csrf,
		map[string]string{"path": "sub"}, "notes.txt", []byte("hello"))
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects/proj/files?") || !strings.Contains(loc, "ok=") || !strings.Contains(loc, "path=sub") {
		t.Fatalf("Location = %q", loc)
	}
	// Describe (overwrite unchecked) then cp; cp target carries prefix+path+leaf.
	var sawCp bool
	for _, c := range *calls {
		joined := strings.Join(c, " ")
		if strings.HasPrefix(joined, "storage cp") {
			sawCp = true
			if !strings.HasSuffix(joined, "gs://test-bucket/pre/sub/notes.txt") {
				t.Fatalf("cp args = %q", joined)
			}
		}
	}
	if !sawCp {
		t.Fatalf("no cp call recorded: %v", *calls)
	}
}

func TestFilesUploadRefusedWithoutCapability(t *testing.T) {
	srv, cfg, calls := storageServer(t)
	// member-1 mapped to operator: file-only support, deliberately read-only here.
	cfg.Projects["proj"] = func() config.ProjectConfig {
		pc := cfg.Projects["proj"]
		pc.CapabilityByUser = map[string]string{"member-1": "operator"}
		return pc
	}()
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFilesMultipart(t, srv, "/projects/proj/files/upload", sid, csrf,
		nil, "notes.txt", []byte("hello"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused upload must not reach the runner: %v", *calls)
	}
}

func TestFilesUploadFeatureOff404(t *testing.T) {
	srv, cfg, calls := storageServer(t)
	cfg.WebAuth.Features.Storage = false
	sid, csrf := adminLogin(t, srv)
	w := postFilesMultipart(t, srv, "/projects/proj/files/upload", sid, csrf,
		nil, "notes.txt", []byte("hello"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with storage feature off", w.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("feature-off upload must not reach the runner: %v", *calls)
	}
}

func TestFilesDeleteHappyAndInvalidObject(t *testing.T) {
	srv, _, calls := storageServer(t)
	sid, csrf := adminLogin(t, srv)

	w := postFix(t, srv, "/projects/proj/files/delete", sid, csrf, url.Values{
		"object": {"sub/notes.txt"}, "path": {"sub"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d body=%s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "storage rm gs://test-bucket/pre/sub/notes.txt" {
		t.Fatalf("rm call = %v", *calls)
	}

	*calls = nil
	w = postFix(t, srv, "/projects/proj/files/delete", sid, csrf, url.Values{
		"object": {"evil*"},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("wildcard delete must redirect with err, Location=%q", loc)
	}
	if len(*calls) != 0 {
		t.Fatalf("wildcard object must not reach the runner: %v", *calls)
	}
}

// writeTestFile stands in for gcloud cp writing the downloaded object.
func writeTestFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func TestFilesDownloadStreamsObject(t *testing.T) {
	srv, _, _ := storageServer(t)
	srv.gcsRunner = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "storage objects describe"):
			return []byte(`{"name":"pre/report.pdf","size":"11","updated":"2026-08-01T10:00:00Z","contentType":"application/pdf"}`), nil
		case strings.HasPrefix(joined, "storage cp"):
			// args: storage cp gs://… <dest>
			dest := args[len(args)-1]
			return nil, writeTestFile(dest, []byte("PDF-CONTENT"))
		}
		return nil, fmt.Errorf("unexpected gcloud args: %s", joined)
	}
	sid, _ := adminLogin(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/files/download?object=report.pdf", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="report.pdf"`) {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
	if w.Body.String() != "PDF-CONTENT" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestFilesDownloadRefusals(t *testing.T) {
	srv, _, _ := storageServer(t)
	sid, _ := adminLogin(t, srv)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	// Missing object (describe answers not-found) → 404.
	if w := get("/projects/proj/files/download?object=gone.txt"); w.Code != http.StatusNotFound {
		t.Fatalf("missing object status = %d, want 404", w.Code)
	}
	// Wildcard / traversal names → 400 before any runner call.
	for _, obj := range []string{"a%2A", "..%2Fescape", ""} {
		if w := get("/projects/proj/files/download?object=" + obj); w.Code != http.StatusBadRequest {
			t.Fatalf("object %q status = %d, want 400", obj, w.Code)
		}
	}
	// Oversize object → 413.
	srv.gcsRunner = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		return []byte(`{"name":"pre/huge.bin","size":"999999999999"}`), nil
	}
	if w := get("/projects/proj/files/download?object=huge.bin"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", w.Code)
	}
}

func TestFilesPageDisabledDoesNotList(t *testing.T) {
	srv, cfg, calls := storageServer(t)
	if err := cfg.SetGlobalStorageGCS("company-bucket", "gw", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageDisabled("proj"); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, "Storage disabled for this project") {
		t.Fatal("disabled project must render disabled card")
	}
	if len(*calls) != 0 {
		t.Fatalf("disabled must not call gcs runner: %v", *calls)
	}
}

func TestFilesPageInheritsGlobalJoinedPrefix(t *testing.T) {
	srv, cfg, calls := storageServer(t)
	if err := cfg.ClearProjectStorage("proj"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetGlobalStorageGCS("company-bucket", "gw", "/etc/keys/svc.json"); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, "company-bucket") {
		t.Fatal("inherited page must show global bucket")
	}
	if !strings.Contains(body, "via global default") {
		t.Fatal("inherited chrome missing")
	}
	if len(*calls) != 1 {
		t.Fatalf("want one ls call, got %v", *calls)
	}
	joined := strings.Join((*calls)[0], " ")
	if !strings.Contains(joined, "gs://company-bucket/gw/proj/") {
		t.Fatalf("ls must use joined prefix: %s", joined)
	}
}

func TestSetProjectStoragePersistsAndRedirects(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gcs"},
		"gcsBucket": {"other-bucket"}, "prefix": {"files/"},
		"credentialsFile": {"/etc/keys/other.json"},
	})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/config/projects/proj/integrations?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	st := cfg.ProjectStorage("proj")
	if st == nil || st.Backend != config.StorageBackendGCS || st.GCSBucket != "other-bucket" || st.Prefix != "files" || st.CredentialsFile != "/etc/keys/other.json" {
		t.Fatalf("stored = %+v", st)
	}
	// Invalid bucket → err= redirect, config unchanged.
	w = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gcs"}, "gcsBucket": {"Bad*Bucket"},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("invalid bucket Location = %q", loc)
	}
	if st := cfg.ProjectStorage("proj"); st == nil || st.GCSBucket != "other-bucket" {
		t.Fatalf("config changed on invalid input: %+v", st)
	}
	// Relative credentials path → err= redirect, config unchanged.
	w = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gcs"},
		"gcsBucket": {"other-bucket"}, "credentialsFile": {"keys/svc.json"},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("relative credentials Location = %q", loc)
	}
	if st := cfg.ProjectStorage("proj"); st == nil || st.CredentialsFile != "/etc/keys/other.json" {
		t.Fatalf("config changed on invalid credentials path: %+v", st)
	}
	// Empty bucket on save is rejected (must not clear).
	w = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gcs"}, "gcsBucket": {""},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("empty save Location = %q", loc)
	}
	if st := cfg.ProjectStorage("proj"); st == nil || st.GCSBucket != "other-bucket" {
		t.Fatalf("empty save cleared storage: %+v", st)
	}
	// Clear via action=clear.
	if err := cfg.SetGlobalStorageGCS("company-bucket", "gw", ""); err != nil {
		t.Fatal(err)
	}
	_ = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"clear"},
	})
	if st := cfg.ProjectStorage("proj"); st != nil {
		t.Fatalf("clear did not nil raw: %+v", st)
	}
	if eff := cfg.EffectiveStorage("proj"); eff == nil || eff.Prefix != "gw/proj" {
		t.Fatalf("clear under global should re-inherit: %+v", eff)
	}
	// Disable ≠ clear.
	_ = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"disable"},
	})
	raw := cfg.ProjectStorage("proj")
	if raw == nil || !raw.Disabled {
		t.Fatalf("disable raw = %+v", raw)
	}
	if cfg.EffectiveStorage("proj") != nil {
		t.Fatal("disable must yield nil effective")
	}
}

func TestSetGlobalStoragePersistsAndRedirects(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	// Page marker + backend picker.
	body := getAuthed(t, srv, "/config/storage", sid)
	if !strings.Contains(body, `id="page-config-storage"`) {
		t.Fatal("global storage page marker missing")
	}
	if !strings.Contains(body, `name="backend"`) || !strings.Contains(body, "Google Drive") {
		t.Fatal("backend picker missing")
	}
	w := postFix(t, srv, "/config/storage", sid, csrf, url.Values{
		"backend": {"gcs"}, "gcsBucket": {"company-bucket"}, "prefix": {"gw"}, "credentialsFile": {"/etc/keys/g.json"},
	})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/config/storage?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	st := cfg.GlobalStorage()
	if st == nil || st.Backend != config.StorageBackendGCS || st.GCSBucket != "company-bucket" || st.Prefix != "gw" {
		t.Fatalf("global = %+v", st)
	}
	// Hub drill.
	hub := getAuthed(t, srv, "/config", sid)
	if !strings.Contains(hub, `href="/config/storage"`) || !strings.Contains(hub, "company-bucket") {
		t.Fatal("hub missing storage drill")
	}
	// Member cannot POST.
	msid, mcsrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w = postFix(t, srv, "/config/storage", msid, mcsrf, url.Values{
		"backend": {"gcs"}, "gcsBucket": {"evil-bucket"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("member global storage status = %d, want 403", w.Code)
	}
	if st := cfg.GlobalStorage(); st == nil || st.GCSBucket != "company-bucket" {
		t.Fatalf("member write changed global: %+v", st)
	}
}

// fakeFilesBackend records List/Upload/Delete/Describe/Download for Drive tests.
type fakeFilesBackend struct {
	mu      sync.Mutex
	lists   []filestore.Target
	uploads []struct {
		t         filestore.Target
		object    string
		overwrite bool
	}
	deletes   []string
	describes []string
	// listEntries returned by List (nil → empty).
	listEntries []filestore.Entry
	// describe returns this when object matches describeObject.
	describeObject string
	describeEntry  filestore.Entry
	describeOK     bool
	downloadBody   []byte
}

func (f *fakeFilesBackend) List(_ context.Context, t filestore.Target, _ string) ([]filestore.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, t)
	return slices.Clone(f.listEntries), nil
}
func (f *fakeFilesBackend) Describe(_ context.Context, _ filestore.Target, object string) (filestore.Entry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describes = append(f.describes, object)
	if f.describeObject != "" && object == f.describeObject {
		return f.describeEntry, f.describeOK, nil
	}
	return filestore.Entry{}, false, nil
}
func (f *fakeFilesBackend) Upload(_ context.Context, _ string, t filestore.Target, object string, overwrite bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, struct {
		t         filestore.Target
		object    string
		overwrite bool
	}{t, object, overwrite})
	return nil
}
func (f *fakeFilesBackend) Download(_ context.Context, _ filestore.Target, object, destPath string) error {
	f.mu.Lock()
	body := f.downloadBody
	f.mu.Unlock()
	if body == nil {
		body = []byte("DRIVE-" + object)
	}
	return os.WriteFile(destPath, body, 0o600)
}
func (f *fakeFilesBackend) Delete(_ context.Context, _ filestore.Target, object string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, object)
	return nil
}

func driveStorageServer(t *testing.T) (*Server, *config.Config, *fakeFilesBackend) {
	t.Helper()
	srv, cfg, _ := authOnServer(t)
	cfg.WebAuth.Features.Storage = true
	if err := cfg.SetProjectStorageDrive("proj", "0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeFilesBackend{
		listEntries: []filestore.Entry{
			{Name: "brief.pdf", Size: 4096, Updated: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)},
			{Name: "docs", IsDir: true},
		},
	}
	srv.filesBackendFn = func(t filestore.Target) (filestore.Backend, error) {
		if t.Backend != filestore.BackendGDrive {
			return nil, fmt.Errorf("test fake: unexpected backend %q", t.Backend)
		}
		return fake, nil
	}
	return srv, cfg, fake
}

func TestFilesPageListsDrive(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, `id="page-project-files"`) {
		t.Fatal("page marker missing")
	}
	for _, want := range []string{"brief.pdf", "4.0 KiB", "docs/", "0ABcdEfghIjKlMnOp", "Drive"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	if len(fake.lists) != 1 {
		t.Fatalf("list calls = %d", len(fake.lists))
	}
	if fake.lists[0].FolderID != "0ABcdEfghIjKlMnOp" || fake.lists[0].IsolationSegment != "" {
		t.Fatalf("list target = %+v", fake.lists[0])
	}
}

func TestFilesPageDisabledNoListDrive(t *testing.T) {
	srv, cfg, fake := driveStorageServer(t)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectStorageDisabled("proj"); err != nil {
		t.Fatal(err)
	}
	fake.lists = nil
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, "Storage disabled for this project") {
		t.Fatal("disabled card missing")
	}
	if len(fake.lists) != 0 {
		t.Fatalf("disabled must not List: %d calls", len(fake.lists))
	}
}

func TestFilesPageInheritsDriveIsolation(t *testing.T) {
	srv, cfg, fake := driveStorageServer(t)
	if err := cfg.ClearProjectStorage("proj"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetGlobalStorageDrive("0AGlobalRootFolder", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	fake.lists = nil
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, "0AGlobalRootFolder") {
		t.Fatal("inherited page must show global folder id")
	}
	if !strings.Contains(body, "via global default") {
		t.Fatal("inherited chrome missing")
	}
	if !strings.Contains(body, `prefix <span class="mono">proj</span>`) {
		t.Fatal("inherited chrome must show the project prefix folder")
	}
	if len(fake.lists) != 1 {
		t.Fatalf("want one list, got %d", len(fake.lists))
	}
	got := fake.lists[0]
	if got.FolderID != "0AGlobalRootFolder" || got.IsolationSegment != "proj" {
		t.Fatalf("inherit target = %+v", got)
	}
}

func TestFilesPageNoBucketExplainer(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	if err := cfg.ClearProjectStorage("proj"); err != nil {
		t.Fatal(err)
	}
	// Ensure no global either — identity-based empty state.
	if err := cfg.SetGlobalStorage(config.StorageInput{}); err != nil {
		t.Fatal(err)
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if !strings.Contains(body, "No file storage linked") {
		t.Fatal("unlinked project must render the explainer card")
	}
	if !strings.Contains(body, "/config/projects/proj/integrations") {
		t.Fatal("admin viewer should see the Integrations settings link")
	}
	if !strings.Contains(body, "/config/storage") {
		t.Fatal("admin viewer should see the global storage link")
	}
	if strings.Contains(body, "test-bucket") {
		t.Fatal("cleared project must not show old bucket")
	}
}

func TestSetGlobalStorageDrivePersistsAndClearIsIdentityBased(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/config/storage", sid, csrf, url.Values{
		"backend":         {"gdrive"},
		"driveFolderId":   {"0ABcdEfghIjKlMnOp"},
		"credentialsFile": {"/etc/keys/drive.json"},
		// Empty gcsBucket must NOT mean cleared (K17).
		"gcsBucket": {""},
		"prefix":    {""},
	})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/config/storage?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	if strings.Contains(loc, "Cleared") || strings.Contains(loc, "cleared") {
		// Flash is URL-encoded; "Updated" is expected, not "Cleared".
		t.Fatalf("Drive save must not flash cleared: %q", loc)
	}
	st := cfg.GlobalStorage()
	if st == nil || st.Backend != config.StorageBackendGDrive || st.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("global drive = %+v", st)
	}
	if st.GCSBucket != "" {
		t.Fatalf("Drive block must strip bucket, got %q", st.GCSBucket)
	}
	// Hub shows Drive identity.
	hub := getAuthed(t, srv, "/config", sid)
	if !strings.Contains(hub, "0ABcdEfghIjKlMnOp") || !strings.Contains(hub, "Drive") {
		t.Fatal("hub missing Drive drill")
	}
	// Clear empties all identity fields → GlobalStorage nil.
	w = postFix(t, srv, "/config/storage", sid, csrf, url.Values{
		"backend": {""}, "gcsBucket": {""}, "prefix": {""},
		"driveFolderId": {""}, "credentialsFile": {""},
	})
	loc = w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("clear Location = %q", loc)
	}
	if cfg.GlobalStorage() != nil {
		t.Fatalf("clear must nil global: %+v", cfg.GlobalStorage())
	}
}

func TestSetProjectStorageDriveInheritsEmptyCredentials(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gdrive"},
		"driveFolderId": {"1OverrideFolderID"},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	st := cfg.ProjectStorage("proj")
	if st == nil || st.Backend != config.StorageBackendGDrive || st.DriveFolderID != "1OverrideFolderID" {
		t.Fatalf("project drive = %+v", st)
	}
	if st.CredentialsFile != "" {
		t.Fatalf("stored credentials must stay empty, got %q", st.CredentialsFile)
	}
	eff := cfg.EffectiveStorage("proj")
	if eff == nil || eff.CredentialsFile != "/etc/keys/drive.json" {
		t.Fatalf("effective creds = %+v", eff)
	}
	// Integrations form still shows empty (raw), not the inherited path.
	body := getAuthed(t, srv, "/config/projects/proj/integrations", sid)
	if strings.Contains(body, "(required)") {
		t.Fatal("project credentials must not be marked required")
	}
	if !strings.Contains(body, "Empty uses the global file-storage credentials") {
		t.Fatal("project credentials help should mention global fallback")
	}
}

func TestSetProjectStorageDriveSameFolderIsolates(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	if err := cfg.SetGlobalStorageDrive("0ABcdEfghIjKlMnOp", "/etc/keys/drive.json"); err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "action": {"save"}, "backend": {"gdrive"},
		"driveFolderId":   {"0ABcdEfghIjKlMnOp"},
		"credentialsFile": {"/etc/keys/drive.json"},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	decoded, _ := url.QueryUnescape(loc)
	if strings.Contains(decoded, "Warning") || strings.Contains(decoded, "same Drive folder") {
		t.Fatalf("same-folder override is isolated; must not warn: %q", decoded)
	}
	st := cfg.ProjectStorage("proj")
	if st == nil || st.Backend != config.StorageBackendGDrive || st.DriveFolderID != "0ABcdEfghIjKlMnOp" {
		t.Fatalf("project drive = %+v", st)
	}
	if st.IsolationSegment != "" {
		t.Fatalf("raw must not persist isolation: %+v", st)
	}
	eff := cfg.EffectiveStorage("proj")
	if eff == nil || eff.IsolationSegment != "proj" {
		t.Fatalf("same-folder override must isolate: %+v", eff)
	}
}

func TestFilesUploadDrivePassesOverwrite(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFilesMultipart(t, srv, "/projects/proj/files/upload", sid, csrf,
		map[string]string{"path": "docs", "overwrite": "1"}, "notes.txt", []byte("hello"))
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("uploads = %+v", fake.uploads)
	}
	if fake.uploads[0].object != "docs/notes.txt" || !fake.uploads[0].overwrite {
		t.Fatalf("upload = %+v", fake.uploads[0])
	}
}

