package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// staticToken always returns the same bearer token.
type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type recordedReq struct {
	Method string
	URL    *url.URL
	Body   []byte
	Header http.Header
}

// fakeDrive is an in-memory Drive REST fake for contract tests.
type fakeDrive struct {
	mu   sync.Mutex
	reqs []recordedReq
	// files[id] = meta; children[parent] = []ids
	files    map[string]fileMeta
	children map[string][]string
	// media[id] = bytes
	media map[string][]byte
	// exportTooLarge[id] makes files.export return 403 exportSizeLimitExceeded.
	exportTooLarge map[string]bool
	// createCount tracks folder creates for race tests
	createCount atomic.Int32
	// afterCreateHooks runs after each create (for race injection)
	onCreateFolder func(name, parent string)
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{
		files:    map[string]fileMeta{},
		children: map[string][]string{},
		media:    map[string][]byte{},
	}
}

func (f *fakeDrive) addFile(parent, id string, meta fileMeta, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta.ID = id
	f.files[id] = meta
	f.children[parent] = append(f.children[parent], id)
	if data != nil {
		f.media[id] = data
	}
}

func (f *fakeDrive) transport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedReq{
			Method: r.Method,
			URL:    r.URL,
			Body:   append([]byte(nil), body...),
			Header: r.Header.Clone(),
		})
		f.mu.Unlock()

		path := r.URL.Path
		q := r.URL.Query()

		// Auth check
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return jsonResponse(401, map[string]any{"error": map[string]any{"message": "unauth"}}), nil
		}

		switch {
		case r.Method == http.MethodGet && path == "/drive/v3/files" && q.Get("alt") != "media":
			return f.handleList(q)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/") && strings.HasSuffix(path, "/export"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/export")
			if decoded, err := url.PathUnescape(id); err == nil {
				id = decoded
			}
			return f.handleExport(id, q)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/"):
			id := strings.TrimPrefix(path, "/drive/v3/files/")
			if q.Get("alt") == "media" {
				return f.handleDownload(id)
			}
			return f.handleGet(id)
		case r.Method == http.MethodPost && path == "/drive/v3/files":
			return f.handleCreateFolder(body)
		case r.Method == http.MethodPost && path == "/upload/drive/v3/files":
			return f.handleUploadCreate(r, body)
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/upload/drive/v3/files/"):
			id := strings.TrimPrefix(path, "/upload/drive/v3/files/")
			return f.handleUploadUpdate(id, body)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/drive/v3/files/"):
			id := strings.TrimPrefix(path, "/drive/v3/files/")
			return f.handleDelete(id)
		default:
			return jsonResponse(404, map[string]any{"error": map[string]any{"message": "no route " + r.Method + " " + path}}), nil
		}
	})
}

func (f *fakeDrive) handleList(q url.Values) (*http.Response, error) {
	// Parse parent from q: 'parent' in parents ...
	query := q.Get("q")
	parent := extractQuotedAfter(query, "in parents")
	// Actually format is: 'PARENT' in parents
	parent = extractFirstQuoted(query)
	nameFilter := ""
	if i := strings.Index(query, "name = '"); i >= 0 {
		rest := query[i+len("name = '"):]
		nameFilter, _ = unquoteDrive(rest)
	}
	foldersOnly := strings.Contains(query, "mimeType = '"+folderMIME+"'")

	// Contract flags
	if q.Get("supportsAllDrives") != "true" || q.Get("includeItemsFromAllDrives") != "true" {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "missing all-drives flags"}}), nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var files []fileMeta
	for _, id := range f.children[parent] {
		m, ok := f.files[id]
		if !ok {
			continue
		}
		if nameFilter != "" && m.Name != nameFilter {
			continue
		}
		if foldersOnly && m.MimeType != folderMIME {
			continue
		}
		files = append(files, m)
	}
	// orderBy=createdTime → sort by created
	if q.Get("orderBy") == "createdTime" {
		// simple bubble by CreatedTime string (ISO sorts lexically)
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				if files[j].CreatedTime < files[i].CreatedTime {
					files[i], files[j] = files[j], files[i]
				}
			}
		}
	}
	return jsonResponse(200, map[string]any{"files": files}), nil
}

func extractFirstQuoted(s string) string {
	i := strings.Index(s, "'")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func extractQuotedAfter(s, _ string) string { return extractFirstQuoted(s) }

func unquoteDrive(s string) (string, string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if s[i] == '\'' {
			return b.String(), s[i+1:]
		}
		b.WriteByte(s[i])
	}
	return b.String(), ""
}

func (f *fakeDrive) handleGet(id string) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.files[id]
	if !ok {
		return jsonResponse(404, map[string]any{"error": map[string]any{"message": "not found"}}), nil
	}
	return jsonResponse(200, m), nil
}

func (f *fakeDrive) handleDownload(id string) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.media[id]
	if !ok {
		if _, exists := f.files[id]; !exists {
			return jsonResponse(404, map[string]any{"error": map[string]any{"message": "not found"}}), nil
		}
		data = nil
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}, nil
}

var fakeExportPDF = []byte("%PDF-1.1\n%%EOF\n")

func (f *fakeDrive) handleExport(id string, q url.Values) (*http.Response, error) {
	if q.Get("mimeType") != "application/pdf" {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "want pdf"}}), nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.files[id]
	if !ok {
		return jsonResponse(404, map[string]any{"error": map[string]any{"message": "not found"}}), nil
	}
	if f.exportTooLarge[id] {
		return jsonResponse(403, map[string]any{
			"error": map[string]any{
				"code":    403,
				"message": "This file is too large to be exported.",
				"errors": []map[string]any{{
					"reason":  "exportSizeLimitExceeded",
					"message": "This file is too large to be exported.",
				}},
			},
		}), nil
	}
	if !ExportsAsPDF(m.MimeType) {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "not exportable"}}), nil
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/pdf"}},
		Body:       io.NopCloser(bytes.NewReader(fakeExportPDF)),
	}, nil
}

func (f *fakeDrive) handleCreateFolder(body []byte) (*http.Response, error) {
	var in struct {
		Name     string   `json:"name"`
		MimeType string   `json:"mimeType"`
		Parents  []string `json:"parents"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": err.Error()}}), nil
	}
	if in.MimeType != folderMIME {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "not a folder create"}}), nil
	}
	parent := ""
	if len(in.Parents) > 0 {
		parent = in.Parents[0]
	}
	n := f.createCount.Add(1)
	id := fmt.Sprintf("folder-%s-%d", in.Name, n)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// For race tests: first create is "older"
	if n == 1 {
		now = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	} else {
		now = time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	}
	meta := fileMeta{ID: id, Name: in.Name, MimeType: folderMIME, CreatedTime: now}
	f.mu.Lock()
	f.files[id] = meta
	f.children[parent] = append(f.children[parent], id)
	hook := f.onCreateFolder
	f.mu.Unlock()
	if hook != nil {
		hook(in.Name, parent)
	}
	return jsonResponse(200, meta), nil
}

func (f *fakeDrive) handleUploadCreate(r *http.Request, body []byte) (*http.Response, error) {
	if r.URL.Query().Get("uploadType") != "multipart" {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "want multipart"}}), nil
	}
	if r.URL.Query().Get("supportsAllDrives") != "true" {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": "supportsAllDrives"}}), nil
	}
	meta, data, err := parseMultipartUpload(r.Header.Get("Content-Type"), body)
	if err != nil {
		return jsonResponse(400, map[string]any{"error": map[string]any{"message": err.Error()}}), nil
	}
	parent := ""
	if len(meta.Parents) > 0 {
		parent = meta.Parents[0]
	}
	id := fmt.Sprintf("file-%s", meta.Name)
	// uniqueness if collision
	f.mu.Lock()
	if _, exists := f.files[id]; exists {
		id = fmt.Sprintf("file-%s-%d", meta.Name, len(f.files))
	}
	m := fileMeta{
		ID: id, Name: meta.Name, MimeType: meta.MimeType,
		Size:         fmt.Sprintf("%d", len(data)),
		ModifiedTime: time.Now().UTC().Format(time.RFC3339),
	}
	f.files[id] = m
	f.children[parent] = append(f.children[parent], id)
	f.media[id] = data
	f.mu.Unlock()
	return jsonResponse(200, m), nil
}

func (f *fakeDrive) handleUploadUpdate(id string, body []byte) (*http.Response, error) {
	// Accept multipart or raw media
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.files[id]
	if !ok {
		return jsonResponse(404, map[string]any{"error": map[string]any{"message": "not found"}}), nil
	}
	// For multipart, extract data part; for simplicity take everything after second blank line
	data := body
	if idx := bytes.LastIndex(body, []byte("\r\n\r\n")); idx >= 0 {
		// find last content part
		parts := bytes.Split(body, []byte("--"+multipartBoundary))
		for _, p := range parts {
			if bytes.Contains(p, []byte("Content-Type: application/json")) {
				continue
			}
			if i := bytes.Index(p, []byte("\r\n\r\n")); i >= 0 {
				chunk := p[i+4:]
				chunk = bytes.TrimSuffix(chunk, []byte("\r\n"))
				chunk = bytes.TrimSuffix(chunk, []byte("--"))
				if len(chunk) > 0 && !bytes.HasPrefix(bytes.TrimSpace(chunk), []byte("{")) {
					data = chunk
					break
				}
			}
		}
	}
	f.media[id] = data
	m.Size = fmt.Sprintf("%d", len(data))
	m.ModifiedTime = time.Now().UTC().Format(time.RFC3339)
	f.files[id] = m
	return jsonResponse(200, m), nil
}

func (f *fakeDrive) handleDelete(id string) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[id]; !ok {
		return jsonResponse(404, map[string]any{"error": map[string]any{"message": "not found"}}), nil
	}
	delete(f.files, id)
	delete(f.media, id)
	for p, ids := range f.children {
		var next []string
		for _, x := range ids {
			if x != id {
				next = append(next, x)
			}
		}
		f.children[p] = next
	}
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
}

type uploadMeta struct {
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Parents  []string `json:"parents"`
}

func parseMultipartUpload(contentType string, body []byte) (uploadMeta, []byte, error) {
	var meta uploadMeta
	// Expect boundary=grokwork_boundary
	if !strings.Contains(contentType, multipartBoundary) {
		return meta, nil, fmt.Errorf("boundary missing in %q", contentType)
	}
	parts := bytes.Split(body, []byte("--"+multipartBoundary))
	var data []byte
	for _, p := range parts {
		p = bytes.TrimPrefix(p, []byte("\r\n"))
		p = bytes.TrimSuffix(p, []byte("\r\n"))
		if len(p) == 0 || string(p) == "--" || bytes.HasPrefix(p, []byte("--")) {
			continue
		}
		headerEnd := bytes.Index(p, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			continue
		}
		headers := string(p[:headerEnd])
		payload := p[headerEnd+4:]
		payload = bytes.TrimSuffix(payload, []byte("\r\n"))
		if strings.Contains(headers, "application/json") {
			if err := json.Unmarshal(payload, &meta); err != nil {
				return meta, nil, err
			}
		} else {
			data = payload
		}
	}
	return meta, data, nil
}

func testClient(f *fakeDrive) *Client {
	httpClient := &http.Client{Transport: f.transport(), Timeout: 5 * time.Second}
	return &Client{
		Auth:       staticToken("test-token"),
		HTTP:       httpClient,
		MediaHTTP:  httpClient,
		APIBase:    "https://www.googleapis.com/drive/v3",
		UploadBase: "https://www.googleapis.com/upload/drive/v3",
	}
}

func TestListFolderNameContainingSlash(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "d1", fileMeta{Name: "a/b", MimeType: folderMIME}, nil)
	f.addFile("d1", "f1", fileMeta{Name: "inside.txt", MimeType: "text/plain", Size: "1"}, []byte("x"))
	c := testClient(f)
	// Encoded hop: one folder whose name is a/b.
	entries, err := c.List(t.Context(), Target{FolderID: "root1"}, "a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "inside.txt" {
		t.Fatalf("encoded slash folder entries = %+v", entries)
	}
	// Naive split would look for folder "a" then "b" and miss.
	_, err = c.List(t.Context(), Target{FolderID: "root1"}, "a/b")
	if err == nil || !strings.Contains(err.Error(), "parent folder") {
		t.Fatalf("unencoded a/b must not walk into a%%2Fb: %v", err)
	}
}

func TestListOneLevel(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "f1", fileMeta{Name: "readme.txt", MimeType: "text/plain", Size: "12", ModifiedTime: "2026-08-01T12:00:00.000Z"}, []byte("hello world!"))
	f.addFile("root1", "d1", fileMeta{Name: "docs", MimeType: folderMIME, CreatedTime: "2026-01-01T00:00:00Z"}, nil)
	c := testClient(f)
	entries, err := c.List(t.Context(), Target{FolderID: "root1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d", len(entries))
	}
	// Contract: list query params
	f.mu.Lock()
	var listReq *recordedReq
	for i := range f.reqs {
		if f.reqs[i].Method == http.MethodGet && f.reqs[i].URL.Path == "/drive/v3/files" {
			listReq = &f.reqs[i]
			break
		}
	}
	f.mu.Unlock()
	if listReq == nil {
		t.Fatal("no list request")
	}
	q := listReq.URL.Query()
	if q.Get("supportsAllDrives") != "true" || q.Get("includeItemsFromAllDrives") != "true" {
		t.Fatalf("all-drives flags: %v", q)
	}
	if q.Get("pageSize") != "200" {
		t.Fatalf("pageSize = %q", q.Get("pageSize"))
	}
	if !strings.Contains(q.Get("q"), "'root1' in parents") {
		t.Fatalf("q = %q", q.Get("q"))
	}
	if q.Get("orderBy") != "folder,name" {
		t.Fatalf("orderBy = %q", q.Get("orderBy"))
	}
}

func TestIsGoogleNativeMIME(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mime string
		want bool
	}{
		{"application/vnd.google-apps.document", true},
		{"APPLICATION/VND.GOOGLE-APPS.FORM", true},
		{"application/vnd.google-apps.shortcut; charset=utf-8", true},
		{"application/vnd.google-apps.folder", false},
		{"application/pdf", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isGoogleNativeMIME(tc.mime); got != tc.want {
			t.Errorf("isGoogleNativeMIME(%q)=%v want %v", tc.mime, got, tc.want)
		}
	}
}

func TestExportsAsPDF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mime string
		want bool
	}{
		{"application/vnd.google-apps.document", true},
		{"application/vnd.google-apps.spreadsheet", true},
		{"application/vnd.google-apps.presentation", true},
		{"application/vnd.google-apps.drawing", true},
		{"APPLICATION/VND.GOOGLE-APPS.DOCUMENT", true},
		{"application/vnd.google-apps.document; charset=utf-8", true},
		{"application/vnd.google-apps.form", false},
		{"application/vnd.google-apps.shortcut", false},
		{"application/vnd.google-apps.folder", false},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ExportsAsPDF(tc.mime); got != tc.want {
			t.Errorf("ExportsAsPDF(%q)=%v want %v", tc.mime, got, tc.want)
		}
	}
}

func TestDownloadExportsNativeAsPDF(t *testing.T) {
	types := []struct {
		name, mime, id string
	}{
		{"Doc", "application/vnd.google-apps.document", "doc1"},
		{"Sheet", "application/vnd.google-apps.spreadsheet", "sheet1"},
		{"Deck", "application/vnd.google-apps.presentation", "deck1"},
		{"Sketch", "application/vnd.google-apps.drawing", "draw1"},
	}
	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDrive()
			f.addFile("root1", tc.id, fileMeta{Name: tc.name, MimeType: tc.mime, Size: "0"}, nil)
			c := testClient(f)
			dest := filepath.Join(t.TempDir(), "out")
			if err := c.Download(t.Context(), Target{FolderID: "root1"}, tc.name, dest); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != string(fakeExportPDF) {
				t.Fatalf("got %q", raw)
			}
			f.mu.Lock()
			var exp *recordedReq
			for i := range f.reqs {
				if strings.HasSuffix(f.reqs[i].URL.Path, "/export") {
					exp = &f.reqs[i]
				}
			}
			f.mu.Unlock()
			if exp == nil {
				t.Fatal("no export request")
			}
			if exp.Method != http.MethodGet {
				t.Fatalf("method = %s", exp.Method)
			}
			if !strings.HasSuffix(exp.URL.Path, "/files/"+tc.id+"/export") {
				t.Fatalf("path = %s", exp.URL.Path)
			}
			q := exp.URL.Query()
			if q.Get("mimeType") != "application/pdf" {
				t.Fatalf("mimeType = %q", q.Get("mimeType"))
			}
			if q.Get("supportsAllDrives") != "" {
				t.Fatalf("export must not send supportsAllDrives, got %q", q.Get("supportsAllDrives"))
			}
		})
	}
}

func TestDownloadRefusesUnexportableNative(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "form1", fileMeta{
		Name: "Intake", MimeType: "application/vnd.google-apps.form", Size: "0",
	}, nil)
	f.addFile("root1", "sc1", fileMeta{
		Name: "Link", MimeType: "application/vnd.google-apps.shortcut", Size: "0",
	}, nil)
	c := testClient(f)
	for _, name := range []string{"Intake", "Link"} {
		err := c.Download(t.Context(), Target{FolderID: "root1"}, name, filepath.Join(t.TempDir(), "out"))
		if err == nil || !strings.Contains(err.Error(), "cannot export") {
			t.Fatalf("%s: want cannot export, got %v", name, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.HasSuffix(r.URL.Path, "/export") {
			t.Fatalf("unexportable native hit /export: %s", r.URL.Path)
		}
	}
}

func TestDownloadExportSizeLimit(t *testing.T) {
	f := newFakeDrive()
	f.exportTooLarge = map[string]bool{"doc1": true}
	f.addFile("root1", "doc1", fileMeta{
		Name: "Huge", MimeType: "application/vnd.google-apps.document", Size: "0",
	}, nil)
	c := testClient(f)
	err := c.Download(t.Context(), Target{FolderID: "root1"}, "Huge", filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	if !strings.Contains(err.Error(), "too large to export") {
		t.Fatalf("want 10 MB copy, got %v", err)
	}
	if strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("size limit must not look like IAM: %v", err)
	}
}

func TestDeleteRefusesFolder(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "d1", fileMeta{Name: "docs", MimeType: folderMIME}, nil)
	c := testClient(f)
	err := c.Delete(t.Context(), Target{FolderID: "root1"}, "docs")
	if err == nil || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("want folder refuse, got %v", err)
	}
}

func TestDeleteFile(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "f1", fileMeta{Name: "a.txt", MimeType: "text/plain", Size: "1"}, []byte("x"))
	c := testClient(f)
	if err := c.Delete(t.Context(), Target{FolderID: "root1"}, "a.txt"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, ok := f.files["f1"]
	f.mu.Unlock()
	if ok {
		t.Fatal("file still present")
	}
	// Contract: DELETE supportsAllDrives
	f.mu.Lock()
	var del *recordedReq
	for i := range f.reqs {
		if f.reqs[i].Method == http.MethodDelete {
			del = &f.reqs[i]
		}
	}
	f.mu.Unlock()
	if del == nil || del.URL.Query().Get("supportsAllDrives") != "true" {
		t.Fatalf("delete req = %+v", del)
	}
}

func TestUploadCreateAndOverwriteByID(t *testing.T) {
	f := newFakeDrive()
	c := testClient(f)
	dir := t.TempDir()
	local := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(local, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := Target{FolderID: "root1"}
	if err := c.Upload(t.Context(), local, tgt, "readme.txt", false); err != nil {
		t.Fatal(err)
	}
	// Second upload without overwrite fails
	if err := os.WriteFile(local, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := c.Upload(t.Context(), local, tgt, "readme.txt", false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want exists error, got %v", err)
	}
	// Overwrite updates same id
	f.mu.Lock()
	var idBefore string
	for id, m := range f.files {
		if m.Name == "readme.txt" {
			idBefore = id
		}
	}
	f.mu.Unlock()
	if err := c.Upload(t.Context(), local, tgt, "readme.txt", true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var ids []string
	for id, m := range f.files {
		if m.Name == "readme.txt" && m.MimeType != folderMIME {
			ids = append(ids, id)
		}
	}
	data := append([]byte(nil), f.media[idBefore]...)
	f.mu.Unlock()
	if len(ids) != 1 || ids[0] != idBefore {
		t.Fatalf("overwrite should update id %q, got %v", idBefore, ids)
	}
	if string(data) != "v2" {
		t.Fatalf("media = %q", data)
	}
	// Contract: PATCH upload path
	f.mu.Lock()
	var patch *recordedReq
	for i := range f.reqs {
		if f.reqs[i].Method == http.MethodPatch {
			patch = &f.reqs[i]
		}
	}
	f.mu.Unlock()
	if patch == nil {
		t.Fatal("no patch")
	}
	if !strings.Contains(patch.URL.Path, "/upload/drive/v3/files/"+idBefore) {
		t.Fatalf("patch path = %s", patch.URL.Path)
	}
	if patch.URL.Query().Get("uploadType") != "multipart" {
		t.Fatalf("uploadType = %q", patch.URL.Query().Get("uploadType"))
	}
}

func TestUploadCreatesIntermediateFolders(t *testing.T) {
	f := newFakeDrive()
	c := testClient(f)
	dir := t.TempDir()
	local := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(local, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(t.Context(), local, Target{FolderID: "root1"}, "docs/a/b/file.bin", false); err != nil {
		t.Fatal(err)
	}
	// List missing intermediate still fails closed
	_, err := c.List(t.Context(), Target{FolderID: "root1"}, "nope/nested")
	if err == nil || !strings.Contains(err.Error(), "parent folder") {
		t.Fatalf("want parent missing, got %v", err)
	}
	// Nested list works after create
	entries, err := c.List(t.Context(), Target{FolderID: "root1"}, "docs/a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "file.bin" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestDownloadBinary(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "f1", fileMeta{Name: "a.bin", MimeType: "application/octet-stream", Size: "4"}, []byte("abcd"))
	c := testClient(f)
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := c.Download(t.Context(), Target{FolderID: "root1"}, "a.bin", dest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "abcd" {
		t.Fatalf("got %q", raw)
	}
}

func TestIsolationEnsureRaceOldestWins(t *testing.T) {
	f := newFakeDrive()
	// Simulate race: when first create happens, inject a second sibling with same name.
	// Actually the client creates once then re-lists. We'll pre-seed nothing and
	// use onCreateFolder to add a second folder after the first create returns —
	// but re-list happens after create, so we need both present on re-list.
	// Approach: on first create, immediately add a younger duplicate.
	var once sync.Once
	f.onCreateFolder = func(name, parent string) {
		once.Do(func() {
			// Younger duplicate (createdTime later — handleCreateFolder sets n=2 style;
			// we inject manually with later time).
			id := "folder-dup-young"
			meta := fileMeta{
				ID: id, Name: name, MimeType: folderMIME,
				CreatedTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
			}
			f.mu.Lock()
			f.files[id] = meta
			f.children[parent] = append(f.children[parent], id)
			f.mu.Unlock()
		})
	}
	c := testClient(f)

	// Concurrent ensureIsolationFolder — mid-race callers may briefly disagree
	// (re-list saw only their create); K8b requires oldest-wins cleanup and an
	// unambiguous subsequent resolve, not identical concurrent return values.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, err := c.ensureIsolationFolder(t.Context(), "root1", "api")
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// Younger injected duplicate should have been deleted (or delete attempted).
	f.mu.Lock()
	_, youngExists := f.files["folder-dup-young"]
	var deletes int
	for _, r := range f.reqs {
		if r.Method == http.MethodDelete {
			deletes++
		}
	}
	// Count remaining folders named api
	var apiFolders int
	var remaining []string
	for _, id := range f.children["root1"] {
		if m, ok := f.files[id]; ok && m.Name == "api" && m.MimeType == folderMIME {
			apiFolders++
			remaining = append(remaining, id)
		}
	}
	f.mu.Unlock()
	if youngExists {
		t.Fatal("younger duplicate still present")
	}
	if deletes < 1 {
		t.Fatal("expected delete of younger duplicate")
	}
	if apiFolders != 1 {
		t.Fatalf("api folders left = %d (%v)", apiFolders, remaining)
	}
	// Subsequent resolve is unambiguous and stable.
	id1, err := c.ensureIsolationFolder(t.Context(), "root1", "api")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := c.ensureIsolationFolder(t.Context(), "root1", "api")
	if err != nil || id2 != id1 {
		t.Fatalf("second resolve = %q err=%v want %q", id2, err, id1)
	}
	if id1 != remaining[0] {
		t.Fatalf("resolve %q != remaining %q", id1, remaining[0])
	}
}

func TestIsolationUsedOnList(t *testing.T) {
	f := newFakeDrive()
	// Pre-create isolation child with a file inside
	f.addFile("root1", "iso-api", fileMeta{Name: "api", MimeType: folderMIME, CreatedTime: "2020-01-01T00:00:00Z"}, nil)
	f.addFile("iso-api", "f1", fileMeta{Name: "only-in-iso.txt", MimeType: "text/plain", Size: "1"}, []byte("x"))
	// Also a file on the parent root that must NOT appear
	f.addFile("root1", "f-root", fileMeta{Name: "company-secret.txt", MimeType: "text/plain", Size: "1"}, []byte("z"))
	c := testClient(f)
	entries, err := c.List(t.Context(), Target{FolderID: "root1", IsolationSegment: "api"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "only-in-iso.txt" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestDescribeExistsSingleFile(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "f1", fileMeta{Name: "a.txt", MimeType: "text/plain", Size: "3"}, []byte("abc"))
	c := testClient(f)
	e, ok, err := c.Describe(t.Context(), Target{FolderID: "root1"}, "a.txt")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if e.Name != "a.txt" || e.Size != 3 {
		t.Fatalf("entry = %+v", e)
	}
	_, ok, err = c.Describe(t.Context(), Target{FolderID: "root1"}, "missing.txt")
	if err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

func TestAmbiguousUserPath(t *testing.T) {
	f := newFakeDrive()
	f.addFile("root1", "f1", fileMeta{Name: "dup.txt", MimeType: "text/plain", Size: "1"}, []byte("a"))
	f.addFile("root1", "f2", fileMeta{Name: "dup.txt", MimeType: "text/plain", Size: "1"}, []byte("b"))
	c := testClient(f)
	_, _, err := c.Describe(t.Context(), Target{FolderID: "root1"}, "dup.txt")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguous, got %v", err)
	}
}

func TestMultipartBoundaryShape(t *testing.T) {
	f := newFakeDrive()
	c := testClient(f)
	local := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(local, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(t.Context(), local, Target{FolderID: "root1"}, "x.txt", false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var post *recordedReq
	for i := range f.reqs {
		if f.reqs[i].Method == http.MethodPost && strings.Contains(f.reqs[i].URL.Path, "/upload/") {
			post = &f.reqs[i]
		}
	}
	f.mu.Unlock()
	if post == nil {
		t.Fatal("no upload post")
	}
	ct := post.Header.Get("Content-Type")
	if !strings.Contains(ct, "boundary="+multipartBoundary) {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !bytes.Contains(post.Body, []byte("--"+multipartBoundary)) {
		t.Fatal("body missing boundary markers")
	}
	if !bytes.Contains(post.Body, []byte(`"name":"x.txt"`)) && !bytes.Contains(post.Body, []byte(`"name": "x.txt"`)) {
		// json.Marshal of map uses no spaces; we use fmt %q in meta which produces "name"
		// Our meta is: {"name":"x.txt","parents":["root1"],"mimeType":"text/plain"}
		if !bytes.Contains(post.Body, []byte(`"name":"x.txt"`)) {
			t.Fatalf("meta missing name: %s", post.Body)
		}
	}
}

func TestPermissionDeniedMapping(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(403, map[string]any{"error": map[string]any{"message": "forbidden"}}), nil
	})
	c := &Client{
		Auth:    staticToken("t"),
		HTTP:    &http.Client{Transport: rt},
		APIBase: "https://www.googleapis.com/drive/v3",
	}
	_, err := c.List(t.Context(), Target{FolderID: "root1"}, "")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("got %v", err)
	}
}
