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
	"testing"

	"github.com/acoshift/grokwork/internal/config"
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

func TestFilesPageNoBucketExplainer(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	if err := cfg.SetProjectStorageGCS("proj", "", "", ""); err != nil {
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
}

func TestSetProjectStoragePersistsAndRedirects(t *testing.T) {
	srv, cfg, _ := storageServer(t)
	sid, csrf := adminLogin(t, srv)
	w := postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "gcsBucket": {"other-bucket"}, "prefix": {"files/"},
		"credentialsFile": {"/etc/keys/other.json"},
	})
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/config/projects/proj/integrations?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("Location = %q", loc)
	}
	st := cfg.ProjectStorage("proj")
	if st == nil || st.GCSBucket != "other-bucket" || st.Prefix != "files" || st.CredentialsFile != "/etc/keys/other.json" {
		t.Fatalf("stored = %+v", st)
	}
	// Invalid bucket → err= redirect, config unchanged.
	w = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "gcsBucket": {"Bad*Bucket"},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("invalid bucket Location = %q", loc)
	}
	if st := cfg.ProjectStorage("proj"); st == nil || st.GCSBucket != "other-bucket" {
		t.Fatalf("config changed on invalid input: %+v", st)
	}
	// Relative credentials path → err= redirect, config unchanged.
	w = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "gcsBucket": {"other-bucket"}, "credentialsFile": {"keys/svc.json"},
	})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("relative credentials Location = %q", loc)
	}
	if st := cfg.ProjectStorage("proj"); st == nil || st.CredentialsFile != "/etc/keys/other.json" {
		t.Fatalf("config changed on invalid credentials path: %+v", st)
	}
	// Clearing unlinks.
	_ = postFix(t, srv, "/config/projects/storage", sid, csrf, url.Values{
		"name": {"proj"}, "gcsBucket": {""},
	})
	if st := cfg.ProjectStorage("proj"); st != nil {
		t.Fatalf("clear did not unlink: %+v", st)
	}
}
