package web

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var githubImagePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func TestGitHubImageURLAllowed(t *testing.T) {
	ok := []string{
		"https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"https://github.com/user-attachments/assets/abcd/",
		"https://www.github.com/user-attachments/assets/abcd",
		"https://github.com/user-attachments/files/12345/shot.png",
		"https://user-images.githubusercontent.com/1/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.png",
		"https://private-user-images.githubusercontent.com/1/x.png?jwt=abc",
		"https://objects.githubusercontent.com/github-production-user-asset-6210df/1/2",
		"https://github-production-user-asset-6210df.s3.amazonaws.com/110723939/636911705-c44c50cd.png",
		"https://github-production-user-asset-6210df.s3.us-east-1.amazonaws.com/1/x.png?X-Amz-Algorithm=AWS4-HMAC-SHA256",
	}
	for _, u := range ok {
		if !githubImageURLAllowed(u) {
			t.Fatalf("allowed: %s", u)
		}
	}
	deny := []string{
		"",
		"http://github.com/user-attachments/assets/abcd",
		"https://github.com/acme/app/raw/main/secret.env",
		"https://github.com/acme/app/archive/refs/heads/main.zip",
		"https://github.com/user-attachments/assets/../secrets",
		"https://github.com/user-attachments/assets/abcd/extra",
		"https://github.com/user-attachments/files/12345/notes.txt",
		"https://github.com:8443/user-attachments/assets/abcd",
		"https://evil.com/user-attachments/assets/abcd",
		"https://camo.githubusercontent.com/abc",
		"https://raw.githubusercontent.com/acme/app/main/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://user:pass@github.com/user-attachments/assets/abcd",
		"https://evil-bucket.s3.amazonaws.com/secret.png",
		"https://github-production-release-asset-6210df.s3.amazonaws.com/1/x.png",
		"https://github-production-user-asset-6210df.s3.amazonaws.com.evil.com/x.png",
		"https://github-production-user-asset-NOTHEX.s3.amazonaws.com/x.png",
	}
	for _, u := range deny {
		if githubImageURLAllowed(u) {
			t.Fatalf("denied: %s", u)
		}
	}
}

func TestPublicIP(t *testing.T) {
	if publicIP(net.ParseIP("10.0.0.1")) || publicIP(net.ParseIP("127.0.0.1")) ||
		publicIP(net.ParseIP("169.254.169.254")) || publicIP(net.ParseIP("::1")) {
		t.Fatal("private/loopback/link-local must fail")
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public v4 must pass")
	}
}

func TestGitHubImageProxyServes(t *testing.T) {
	srv := workflowServer(t)
	var saw string
	srv.githubImageGet = func(_ context.Context, rawURL string) (string, []byte, error) {
		saw = rawURL
		return "image/png", githubImagePNG, nil
	}
	const asset = "https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/github-images?u="+asset, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if saw != asset {
		t.Fatalf("fetched %q", saw)
	}
	if w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("ctype=%q", w.Header().Get("Content-Type"))
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if !bytes.Equal(w.Body.Bytes(), githubImagePNG) {
		t.Fatalf("body mismatch len=%d", len(w.Body.Bytes()))
	}
}

func TestGitHubImageProxySniffsPNG(t *testing.T) {
	srv := workflowServer(t)
	srv.githubImageGet = func(context.Context, string) (string, []byte, error) {
		return "application/octet-stream", githubImagePNG, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/github-images?u=https://github.com/user-attachments/assets/abcd", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("sniff ctype=%q", w.Header().Get("Content-Type"))
	}
}

func TestGitHubImageProxyRejectsHTML(t *testing.T) {
	srv := workflowServer(t)
	srv.githubImageGet = func(context.Context, string) (string, []byte, error) {
		return "text/html", []byte("<html>nope</html>"), nil
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/github-images?u=https://github.com/user-attachments/assets/abcd", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestGitHubImageProxyRejectsRawRepoURL(t *testing.T) {
	srv := workflowServer(t)
	called := false
	srv.githubImageGet = func(context.Context, string) (string, []byte, error) {
		called = true
		return "image/png", githubImagePNG, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/github-images?u=https://github.com/acme/app/raw/main/secret.png", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	if called {
		t.Fatal("must not fetch a raw repo url")
	}
}

func TestGitHubImageProxyUnknownProject(t *testing.T) {
	srv := workflowServer(t)
	srv.githubImageGet = func(context.Context, string) (string, []byte, error) {
		t.Fatal("must not fetch")
		return "", nil, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/nope/github-images?u=https://github.com/user-attachments/assets/abcd", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestGitHubMarkdownRewritesAttachment(t *testing.T) {
	srv := workflowServer(t)
	got := string(srv.githubMarkdown("proj", `<img src="https://github.com/user-attachments/assets/abcd" alt="repro">`))
	if !strings.Contains(got, `/projects/proj/github-images?u=https%3A%2F%2Fgithub.com%2Fuser-attachments%2Fassets%2Fabcd`) {
		t.Fatalf("rewrite missing: %s", got)
	}
	if strings.Contains(got, `src="https://github.com/user-attachments`) {
		t.Fatalf("raw github src leaked: %s", got)
	}
}
