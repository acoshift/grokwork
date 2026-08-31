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

func filesPageBody(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `id="page-project-files"`)
	if i < 0 {
		t.Fatal("page marker missing")
	}
	return body[i:]
}

func assertOpenInDrive(t *testing.T, body, href string) {
	t.Helper()
	want := `href="` + href + `" target="_blank" rel="noopener"`
	if !strings.Contains(body, want) {
		t.Fatalf("missing Open in Drive link %q", want)
	}
	if !strings.Contains(body, ">Open in Drive</a>") {
		t.Fatal(`missing label "Open in Drive"`)
	}
}

func TestFilesPageListsBucket(t *testing.T) {
	srv, _, calls := storageServer(t)
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	body = filesPageBody(t, body)
	for _, want := range []string{"report.pdf", "2.0 KiB", "sub/", "test-bucket"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	// Admin (unmapped → builder) may write: upload form renders.
	if !strings.Contains(body, `id="files-upload"`) {
		t.Fatal("upload form missing for builder-class viewer")
	}
	if strings.Contains(body, "Open in Drive") {
		t.Fatal("GCS listing must not render Open in Drive")
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
	slash := fileBreadcrumbs("Docs%2FCustomer")
	if !slices.Equal(slash, []fileCrumb{
		{Label: "Files", Path: ""},
		{Label: "Docs/Customer", Path: "Docs%2FCustomer", Last: true},
	}) {
		t.Fatalf("slash folder crumb = %#v", slash)
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
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="report.pdf"`) ||
		!strings.Contains(got, "attachment") ||
		!strings.Contains(got, "filename*=UTF-8''report.pdf") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
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
	redirectErr := func(path, label string) {
		t.Helper()
		w := get(path)
		if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
			t.Fatalf("%s status = %d, want redirect; body=%s", label, w.Code, w.Body.String())
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s Location = %q", label, loc)
		}
	}
	// Missing object (describe answers not-found) → back to the listing.
	redirectErr("/projects/proj/files/download?object=gone.txt", "missing object")
	// Traversal / empty names are refused before backend I/O.
	for _, obj := range []string{"..%2Fescape", ""} {
		redirectErr("/projects/proj/files/download?object="+obj, "object "+obj)
	}
	// GCS wildcard: web containment allows it; the adapter refuses without gcloud.
	redirectErr("/projects/proj/files/download?object=a%2A", "gcs wildcard")
	// Oversize object → listing flash, not a raw 413 page.
	srv.gcsRunner = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		return []byte(`{"name":"pre/huge.bin","size":"999999999999"}`), nil
	}
	redirectErr("/projects/proj/files/download?object=huge.bin", "oversize")
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
	mu        sync.Mutex
	lists     []filestore.Target
	listPaths []string
	uploads   []struct {
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
	// folderOpenURL is the current-folder Google URL List returns (Drive only).
	folderOpenURL string
}

func (f *fakeFilesBackend) List(_ context.Context, t filestore.Target, subPath string) (filestore.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, t)
	f.listPaths = append(f.listPaths, subPath)
	return filestore.Listing{FolderOpenURL: f.folderOpenURL, Entries: slices.Clone(f.listEntries)}, nil
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
		folderOpenURL: "https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp",
		listEntries: []filestore.Entry{
			{Name: "brief.pdf", Size: 4096, Updated: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), OpenURL: "https://drive.google.com/file/d/fileBrief/view"},
			{Name: "docs", IsDir: true, OpenURL: "https://drive.google.com/drive/folders/folderDocs"},
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

func TestFilesPageSlashFolderHrefAndList(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.listEntries = []filestore.Entry{{Name: "Docs/Customer", IsDir: true}}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if i := strings.Index(body, `id="page-project-files"`); i >= 0 {
		body = body[i:]
	}
	if !strings.Contains(body, `>Docs/Customer/</a>`) {
		t.Fatal("slash folder display name missing")
	}
	// AppendName encodes / as %2F; urlquery then writes %252F in the href.
	if !strings.Contains(body, `path=Docs%252FCustomer`) {
		t.Fatalf("slash folder must be one encoded hop, body missing path=Docs%%252FCustomer")
	}
	fake.listPaths = nil
	inner := getAuthed(t, srv, "/projects/proj/files?path=Docs%252FCustomer", sid)
	if strings.Contains(inner, "invalid path") {
		t.Fatal("encoded slash folder path must list, not fail validation")
	}
	if len(fake.listPaths) != 1 || fake.listPaths[0] != "Docs%2FCustomer" {
		t.Fatalf("List path = %v, want [Docs%%2FCustomer]", fake.listPaths)
	}
}

func TestFilesPageListsDrive(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	sid, _ := adminLogin(t, srv)
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files", sid))
	for _, want := range []string{"brief.pdf", "4.0 KiB", "docs/", "0ABcdEfghIjKlMnOp", "Drive"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	assertOpenInDrive(t, body, "https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp")
	assertOpenInDrive(t, body, "https://drive.google.com/file/d/fileBrief/view")
	assertOpenInDrive(t, body, "https://drive.google.com/drive/folders/folderDocs")
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
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files", sid))
	if !strings.Contains(body, "Storage disabled for this project") {
		t.Fatal("disabled card missing")
	}
	if strings.Contains(body, "Open in Drive") {
		t.Fatal("disabled Files page must not render Open in Drive")
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
	fake.folderOpenURL = "https://drive.google.com/drive/folders/isoChildProj"
	sid, _ := adminLogin(t, srv)
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files", sid))
	if !strings.Contains(body, "0AGlobalRootFolder") {
		t.Fatal("inherited page must show global folder id")
	}
	assertOpenInDrive(t, body, "https://drive.google.com/drive/folders/isoChildProj")
	if strings.Contains(body, `href="https://drive.google.com/drive/folders/0AGlobalRootFolder"`) {
		t.Fatal("inherited listing must Open-in-Drive the isolation child, not the configured parent")
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
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files", sid))
	if !strings.Contains(body, "No file storage linked") {
		t.Fatal("unlinked project must render the explainer card")
	}
	if strings.Contains(body, "Open in Drive") {
		t.Fatal("unconfigured Files page must not render Open in Drive")
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

func TestFilePreviewKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, ctype, want string
	}{
		{"a.png", "image/png", "image"},
		{"a.jpg", "image/jpeg", "image"},
		{"a.pdf", "application/pdf", "pdf"},
		{"a.pdf", "", "pdf"},
		{"a.bin", "application/octet-stream", ""},
		{"a.svg", "image/svg+xml", ""},
		{"a.html", "text/html", ""},
		{"Sheet", "application/vnd.google-apps.spreadsheet", "pdf"},
		{"Doc", "application/vnd.google-apps.document", "pdf"},
		{"Form", "application/vnd.google-apps.form", ""},
		{"photo.png", "text/html", ""},
	}
	for _, tc := range cases {
		if got := filePreviewKind(tc.name, tc.ctype); got != tc.want {
			t.Errorf("filePreviewKind(%q, %q)=%q want %q", tc.name, tc.ctype, got, tc.want)
		}
	}
}

func TestFilesPageDownloadAndPreviewMarkup(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.listEntries = []filestore.Entry{
		{Name: "Report [final].pdf", Size: 4096, ContentType: "application/pdf", OpenURL: "https://drive.google.com/file/d/fileReport/view"},
		{Name: "Sheet", Size: 0, ContentType: "application/vnd.google-apps.spreadsheet", OpenURL: "https://docs.google.com/spreadsheets/d/fileSheet/edit"},
		{Name: "Intake", Size: 0, ContentType: "application/vnd.google-apps.form", OpenURL: "https://docs.google.com/forms/d/fileIntake/edit"},
		{Name: "notes [v2].txt", Size: 12, ContentType: "text/plain", OpenURL: "https://drive.google.com/file/d/fileNotes/view"},
	}
	sid, _ := adminLogin(t, srv)
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files", sid))
	for _, want := range []string{
		`hx-boost="false"`,
		// urlquery emits + for space; html/template then writes &#43; in href.
		`href="/projects/proj/files/download?object=Report&#43;%5Bfinal%5D.pdf"`,
		`href="/projects/proj/files?preview=Report&#43;%5Bfinal%5D.pdf"`,
		`data-src="/projects/proj/files/preview?object=Report&#43;%5Bfinal%5D.pdf"`,
		`data-preview="pdf"`,
		`>Preview</a>`,
		`href="/projects/proj/files/download?object=Sheet"`,
		`href="/projects/proj/files?preview=Sheet"`,
		`>Download PDF</a>`,
		`Google Sheet`,
		`Cannot download`,
		`notes [v2].txt`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	if strings.Contains(body, `/files/download?object=Intake`) {
		t.Fatal("unexportable Google file must not have a Download href")
	}
	if strings.Contains(body, `preview=Intake`) {
		t.Fatal("unexportable Google file must not have a Preview href")
	}
	if strings.Contains(body, "export as PDF to download") {
		t.Fatal("exportable native must not keep the old cannot-download note")
	}
	assertOpenInDrive(t, body, "https://docs.google.com/forms/d/fileIntake/edit")
	assertOpenInDrive(t, body, "https://docs.google.com/spreadsheets/d/fileSheet/edit")
	assertOpenInDrive(t, body, "https://drive.google.com/file/d/fileReport/view")
}

func TestFilesPageNestedFolderOpenInDrive(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.folderOpenURL = "https://drive.google.com/drive/folders/nestedDocsFolder"
	fake.listEntries = []filestore.Entry{
		{Name: "notes.txt", Size: 12, ContentType: "text/plain", OpenURL: "https://drive.google.com/file/d/fileNotes/view"},
	}
	sid, _ := adminLogin(t, srv)
	body := filesPageBody(t, getAuthed(t, srv, "/projects/proj/files?path=docs", sid))
	assertOpenInDrive(t, body, "https://drive.google.com/drive/folders/nestedDocsFolder")
	assertOpenInDrive(t, body, "https://drive.google.com/file/d/fileNotes/view")
	if strings.Contains(body, `href="https://drive.google.com/drive/folders/0ABcdEfghIjKlMnOp"`) {
		t.Fatal("nested listing must not Open-in-Drive the configured parent")
	}
}

func TestFilesPagePreviewQueryOpensModal(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.listEntries = []filestore.Entry{
		{Name: "Report [final].pdf", Size: 4096, ContentType: "application/pdf"},
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files?preview=Report+%5Bfinal%5D.pdf", sid)
	if i := strings.Index(body, `id="page-project-files"`); i >= 0 {
		body = body[i:]
	}
	for _, want := range []string{
		`id="page-project-files"`,
		`data-open="1"`,
		`data-kind="pdf"`,
		// QueryEscape uses +; html/template writes &#43; in the attribute.
		`src="/projects/proj/files/preview?object=Report&#43;%5Bfinal%5D.pdf"`,
		`>Report [final].pdf</h2>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	// The page itself is still the listing — bytes stay on /files/preview for the iframe.
	if !strings.Contains(body, `id="files-preview-dialog"`) {
		t.Fatal("dialog missing")
	}

	plain := getAuthed(t, srv, "/projects/proj/files?preview=notes.txt", sid)
	if strings.Contains(plain, `data-open="1"`) {
		t.Fatal("non-previewable ?preview= must not open the modal")
	}
}

func TestFilesDownloadDriveStreamsObject(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.describeObject = "brief.pdf"
	fake.describeEntry = filestore.Entry{Name: "brief.pdf", Size: 11, ContentType: "application/pdf"}
	fake.describeOK = true
	fake.downloadBody = []byte("PDF-CONTENT")
	sid, _ := adminLogin(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/files/download?object=brief.pdf", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "PDF-CONTENT" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestFilesDownloadDriveNativeExportsPDF(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.describeObject = "Sheet"
	fake.describeEntry = filestore.Entry{
		Name: "Sheet", Size: 0, ContentType: "application/vnd.google-apps.spreadsheet",
	}
	fake.describeOK = true
	fake.downloadBody = []byte("%PDF-1.1 native")
	sid, _ := adminLogin(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/files/download?object=Sheet", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="Sheet.pdf"`) ||
		!strings.Contains(got, "attachment") ||
		!strings.Contains(got, "filename*=UTF-8''Sheet.pdf") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if w.Body.String() != "%PDF-1.1 native" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestFilesDownloadDriveFormRefused(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.describeObject = "Intake"
	fake.describeEntry = filestore.Entry{
		Name: "Intake", Size: 0, ContentType: "application/vnd.google-apps.form",
	}
	fake.describeOK = true
	sid, _ := adminLogin(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/files/download?object=Intake", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want redirect; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestDriveExportsPDF(t *testing.T) {
	t.Parallel()
	if driveExportsPDF(filestore.BackendGCS, "application/vnd.google-apps.document") {
		t.Fatal("GCS must not claim Drive PDF export")
	}
	if !driveExportsPDF(filestore.BackendGDrive, "application/vnd.google-apps.document") {
		t.Fatal("Drive Doc must export as PDF")
	}
	if driveExportsPDF(filestore.BackendGDrive, "application/vnd.google-apps.form") {
		t.Fatal("Drive Form must not export as PDF")
	}
}

func TestFilesGCSNativeMimeNotExported(t *testing.T) {
	srv, _, _ := storageServer(t)
	fake := &fakeFilesBackend{
		listEntries: []filestore.Entry{
			{Name: "Sheet", Size: 0, ContentType: "application/vnd.google-apps.spreadsheet"},
		},
		describeObject: "Sheet",
		describeEntry: filestore.Entry{
			Name: "Sheet", Size: 0, ContentType: "application/vnd.google-apps.spreadsheet",
		},
		describeOK:   true,
		downloadBody: []byte("NOT-A-PDF"),
	}
	srv.filesBackendFn = func(t filestore.Target) (filestore.Backend, error) {
		if t.Backend != filestore.BackendGCS {
			return nil, fmt.Errorf("test fake: unexpected backend %q", t.Backend)
		}
		return fake, nil
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if strings.Contains(body, `/files/download?object=Sheet`) {
		t.Fatal("GCS native mime must not get a Download href")
	}
	if strings.Contains(body, `preview=Sheet`) {
		t.Fatal("GCS native mime must not get a Preview href")
	}
	if strings.Contains(body, "Download PDF") {
		t.Fatal("GCS native mime must not say Download PDF")
	}
	if !strings.Contains(body, "Cannot download") {
		t.Fatal("GCS native mime should keep the cannot-download note")
	}
	if i := strings.Index(body, `id="page-project-files"`); i >= 0 {
		body = body[i:]
	}
	if strings.Contains(body, "Open in Drive") {
		t.Fatal("GCS listing must not render Open in Drive")
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/proj/files/download?object=Sheet", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("download status = %d, want redirect; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("Location = %q", loc)
	}

	req = httptest.NewRequest(http.MethodGet, "/projects/proj/files/preview?object=Sheet", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("preview status = %d, want 415", w.Code)
	}
}

func TestWithPDFExt(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Sheet", "Sheet.pdf"},
		{"Sheet.pdf", "Sheet.pdf"},
		{"Sheet.PDF", "Sheet.PDF"},
		{"", "file.pdf"},
		{"  Q3 Plan  ", "Q3 Plan.pdf"},
	}
	for _, tc := range cases {
		if got := withPDFExt(tc.in); got != tc.want {
			t.Errorf("withPDFExt(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFilesPreviewPDFAndRefusals(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.describeObject = "brief.pdf"
	fake.describeEntry = filestore.Entry{Name: "brief.pdf", Size: 11, ContentType: "application/pdf"}
	fake.describeOK = true
	fake.downloadBody = []byte("%PDF-1.1 test")
	sid, _ := adminLogin(t, srv)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	w := get("/projects/proj/files/preview?object=brief.pdf")
	if w.Code != http.StatusOK {
		t.Fatalf("pdf preview status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "inline") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("preview must not redirect, Location=%q", loc)
	}

	fake.describeObject = "page.html"
	fake.describeEntry = filestore.Entry{Name: "page.html", Size: 12, ContentType: "text/html"}
	w = get("/projects/proj/files/preview?object=page.html")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html preview status = %d, want 415", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("preview 415 must not redirect, Location=%q", loc)
	}

	fake.describeObject = "Sheet"
	fake.describeEntry = filestore.Entry{Name: "Sheet", Size: 0, ContentType: "application/vnd.google-apps.spreadsheet"}
	fake.downloadBody = []byte("%PDF-1.1 sheet")
	w = get("/projects/proj/files/preview?object=Sheet")
	if w.Code != http.StatusOK {
		t.Fatalf("sheet preview status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("sheet preview Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "inline") ||
		!strings.Contains(got, "Sheet.pdf") {
		t.Fatalf("sheet preview Content-Disposition = %q", got)
	}

	fake.describeObject = "Intake"
	fake.describeEntry = filestore.Entry{Name: "Intake", Size: 0, ContentType: "application/vnd.google-apps.form"}
	w = get("/projects/proj/files/preview?object=Intake")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form preview status = %d, want 415", w.Code)
	}
}

func TestFilesDeleteDriveBracketNameReachesBackend(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/projects/proj/files/delete", sid, csrf, url.Values{
		"object": {"Report [final].pdf"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "Report [final].pdf" {
		t.Fatalf("deletes = %v", fake.deletes)
	}
}

func TestFilesUploadMultiple(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	sid, csrf := adminLogin(t, srv)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("path", "docs"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.uploads) != 2 {
		t.Fatalf("uploads = %+v", fake.uploads)
	}
	if fake.uploads[0].object != "docs/a.txt" || fake.uploads[1].object != "docs/b.txt" {
		t.Fatalf("objects = %+v", fake.uploads)
	}
	loc, _ := url.QueryUnescape(w.Header().Get("Location"))
	if !strings.Contains(loc, "Uploaded a.txt, b.txt") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestFileIconKind(t *testing.T) {
	cases := []struct {
		name, ctype string
		dir         bool
		icon, label string
	}{
		{"docs", "", true, "folder", "Folder"},
		{"report.pdf", "application/pdf", false, "pdf", "PDF"},
		{"scan", "application/pdf; charset=binary", false, "pdf", "PDF"},
		{"photo.png", "image/png", false, "image", "PNG image"},
		{"photo.jpeg", "", false, "image", "JPEG image"},
		{"Sheet", "application/vnd.google-apps.spreadsheet", false, "gsheet", "Google Sheet"},
		{"Doc", "application/vnd.google-apps.document", false, "gdoc", "Google Doc"},
		{"Deck", "application/vnd.google-apps.presentation", false, "gslides", "Google Slides"},
		{"Sketch", "application/vnd.google-apps.drawing", false, "gdoc", "Google Drawing"},
		{"Form", "application/vnd.google-apps.form", false, "gdoc", "Google file"},
		{"notes.txt", "text/plain", false, "text", "TXT"},
		{"README", "text/markdown", false, "text", "Text"},
		{"main.go", "", false, "code", "GO"},
		{"bundle.tar.gz", "application/gzip", false, "archive", "Archive"},
		{"clip.mp4", "video/mp4", false, "video", "Video"},
		{"song.mp3", "", false, "audio", "Audio"},
		{"budget.xlsx", "", false, "gsheet", "XLSX"},
		{"blob.bin", "application/octet-stream", false, "file", "BIN"},
		{"noext", "", false, "file", "File"},
		{"weird.verylongext", "", false, "file", "File"},
	}
	for _, tc := range cases {
		icon, label := fileIconKind(tc.name, tc.ctype, tc.dir)
		if icon != tc.icon || label != tc.label {
			t.Errorf("fileIconKind(%q,%q,%v) = (%q,%q), want (%q,%q)", tc.name, tc.ctype, tc.dir, icon, label, tc.icon, tc.label)
		}
	}
}

func TestFileRowMetaLine(t *testing.T) {
	file := fileRow{KindLabel: "PDF", SizeHuman: "4.0 KiB", UpdatedText: "2026-08-01 10:00"}
	if got := file.MetaLine(); got != "PDF · 4.0 KiB · 2026-08-01 10:00" {
		t.Fatalf("file meta = %q", got)
	}
	// A folder reports neither kind nor size — only when it was last touched.
	dir := fileRow{IsDir: true, KindLabel: "Folder", SizeHuman: "0 B", UpdatedText: "2026-08-01 10:00"}
	if got := dir.MetaLine(); got != "2026-08-01 10:00" {
		t.Fatalf("dir meta = %q", got)
	}
	if got := (fileRow{IsDir: true}).MetaLine(); got != "" {
		t.Fatalf("stampless dir meta = %q, want empty", got)
	}
	native := fileRow{KindLabel: "Google Sheet", NativeGoogle: true, SizeHuman: "", UpdatedText: "2026-08-01 10:00"}
	if got := native.MetaLine(); got != "Google Sheet · 2026-08-01 10:00" {
		t.Fatalf("native meta = %q", got)
	}
}

// The listing panel renders a desktop table and a phone .m-rows twin from the
// same rows, prints what the folder holds, and every preview anchor is
// unboosted: htmx never checks defaultPrevented, so a boosted click would
// re-render the listing behind the dialog on every open.
func TestFilesPagePanelAndPhoneTwin(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.listEntries = []filestore.Entry{
		{Name: "docs", IsDir: true},
		{Name: "brief.pdf", Size: 4096, ContentType: "application/pdf", Updated: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)},
		{Name: "shot.png", Size: 1024, ContentType: "image/png"},
		{Name: "notes.txt", Size: 12, ContentType: "text/plain"},
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files", sid)
	if i := strings.Index(body, `id="page-project-files"`); i >= 0 {
		body = body[i:]
	}
	for _, want := range []string{
		`class="section files-panel"`,
		`>All files</h2>`,
		`1 folder`,
		`3 files <span class="muted-soft">(5.0 KiB)</span>`,
		`class="m-rows files-mrows"`,
		`class="table-scroll m-hide files-table"`,
		`class="fico fico-folder"`,
		`class="fico fico-pdf"`,
		`class="fico fico-image"`,
		`class="fico fico-text"`,
		`data-meta="PDF · 4.0 KiB · 2026-08-01 10:00"`,
		`class="files-mrow-hit" href="/projects/proj/files?preview=brief.pdf"`,
		`aria-label="Preview brief.pdf"`,
		`aria-label="Download notes.txt"`,
		`aria-label="Delete brief.pdf"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	// Every preview anchor (name link, Preview button, phone row) is unboosted.
	n := strings.Count(body, `data-preview="`)
	if n == 0 {
		t.Fatal("no preview anchors rendered")
	}
	if got := strings.Count(body, `hx-boost="false" data-preview="`); got != n {
		t.Fatalf("preview anchors unboosted = %d of %d", got, n)
	}
	// Root listing: no one-segment crumb; the panel says where you are.
	if strings.Contains(body, `class="pr-crumb"`) {
		t.Fatal("root listing must not render a crumb")
	}
	// Directories carry no delete form and no meta beyond a stamp.
	if strings.Contains(body, `aria-label="Delete docs"`) {
		t.Fatal("folders must not offer delete")
	}
	// The listing summary is not printed for an empty folder; the empty state is.
	fake.listEntries = nil
	empty := getAuthed(t, srv, "/projects/proj/files?path=docs", sid)
	if !strings.Contains(empty, `This folder is empty.`) || !strings.Contains(empty, `Drop files above to add the first one.`) {
		t.Fatal("empty state missing")
	}
	if !strings.Contains(empty, `>docs</h2>`) {
		t.Fatal("nested panel must title the current folder")
	}
}

// The viewer header carries the file's meta line when ?preview= names a
// listed object, and the header actions keep their labels for script.
func TestFilesPagePreviewQueryFillsMeta(t *testing.T) {
	srv, _, fake := driveStorageServer(t)
	fake.listEntries = []filestore.Entry{
		{Name: "brief.pdf", Size: 4096, ContentType: "application/pdf", Updated: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)},
	}
	sid, _ := adminLogin(t, srv)
	body := getAuthed(t, srv, "/projects/proj/files?preview=brief.pdf", sid)
	for _, want := range []string{
		`id="files-preview-meta" class="files-preview-meta">PDF · 4.0 KiB · 2026-08-01 10:00</span>`,
		`id="files-preview-glyph" data-kind="pdf"`,
		`id="files-preview-open" class="btn-secondary btn-compact files-preview-act" target="_blank" rel="noopener"`,
		`href="/projects/proj/files/preview?object=brief.pdf"`,
		`id="files-preview-prev"`,
		`id="files-preview-next"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	// An object outside the listing still opens by extension, with no meta.
	other := getAuthed(t, srv, "/projects/proj/files?preview=elsewhere.pdf", sid)
	if !strings.Contains(other, `data-open="1"`) {
		t.Fatal("unlisted previewable object must still open")
	}
	if !strings.Contains(other, `id="files-preview-meta" class="files-preview-meta"></span>`) {
		t.Fatal("unlisted object must render an empty meta line, not a guess")
	}
}
