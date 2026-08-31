package filestore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/gdrive"
)

func TestValidateObjectPath(t *testing.T) {
	if err := ValidateObjectPath(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateObjectPath("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateObjectPath("Report [final].pdf"); err != nil {
		t.Fatalf("bracket name: %v", err)
	}
	if err := ValidateObjectPath("x*"); err != nil {
		t.Fatalf("wildcard name is a Drive-legal leaf: %v", err)
	}
	if err := ValidateObjectPath("a%2Fb"); err != nil {
		t.Fatalf("encoded slash name: %v", err)
	}
	for _, bad := range []string{"/x", "a/../b", "a//b", "a/%2e%2e"} {
		if err := ValidateObjectPath(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}

func TestJoinSplitPathRoundTrip(t *testing.T) {
	if got := JoinNames("Docs for Customer", "CR AMB"); got != "Docs for Customer/CR AMB" {
		t.Fatalf("nested = %q", got)
	}
	if got := JoinNames("Docs/Customer"); got != "Docs%2FCustomer" {
		t.Fatalf("slash name = %q", got)
	}
	if got := AppendName("", "a/b"); got != "a%2Fb" {
		t.Fatalf("append root = %q", got)
	}
	if got := AppendName("docs", "a/b"); got != "docs/a%2Fb" {
		t.Fatalf("append = %q", got)
	}
	segs, err := SplitPath("docs/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0] != "docs" || segs[1] != "a/b" {
		t.Fatalf("split = %#v", segs)
	}
	native, err := NativePath("docs/a%2Fb")
	if err != nil || native != "docs/a/b" {
		t.Fatalf("native = %q %v", native, err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := SanitizeFilename("../../etc/passwd"); got == "" || got == ".." {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeFilename("ok-file.txt"); got != "ok-file.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestGDriveListSurfacesOpenURL(t *testing.T) {
	var listURL string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "" {
			t.Fatal("missing bearer")
		}
		if r.URL.Path == "/drive/v3/files" {
			listURL = r.URL.String()
			body, _ := json.Marshal(map[string]any{
				"files": []map[string]string{
					{"id": "f1", "name": "readme.txt", "mimeType": "text/plain", "size": "12"},
					{"id": "d1", "name": "docs", "mimeType": "application/vnd.google-apps.folder"},
				},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(string(body))),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL)
		return nil, nil
	})
	b := GDrive{Client: &gdrive.Client{
		Auth:    staticToken("t"),
		HTTP:    &http.Client{Transport: rt},
		APIBase: "https://www.googleapis.com/drive/v3",
	}}
	listing, err := b.List(t.Context(), Target{Backend: BackendGDrive, FolderID: "root1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if listing.FolderOpenURL != "https://drive.google.com/drive/folders/root1" {
		t.Fatalf("FolderOpenURL = %q", listing.FolderOpenURL)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("len = %d", len(listing.Entries))
	}
	byName := map[string]Entry{}
	for _, e := range listing.Entries {
		byName[e.Name] = e
	}
	if got := byName["readme.txt"].OpenURL; got != "https://drive.google.com/file/d/f1/view" {
		t.Fatalf("readme OpenURL = %q", got)
	}
	if got := byName["docs"].OpenURL; got != "https://drive.google.com/drive/folders/d1" {
		t.Fatalf("docs OpenURL = %q", got)
	}
	if !strings.Contains(listURL, "id") || !strings.Contains(listURL, "webViewLink") {
		t.Fatalf("list request missing id/webViewLink: %s", listURL)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
