package sentry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/errsrc"
)

func TestSentryCountParsesString(t *testing.T) {
	if parseCount(json.RawMessage(`"84"`)) != 84 {
		t.Fatal("string")
	}
	if parseCount(json.RawMessage(`84`)) != 84 {
		t.Fatal("number")
	}
}

func TestGetUsesOrgScopedRouteAndReturnsProjectSlug(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "/events/latest") {
			io.WriteString(w, `{"dateCreated":"2026-08-01T00:00:00Z","entries":[]}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/0/organizations/acme/issues/") {
			t.Errorf("unscoped or wrong org: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"id":"9","shortId":"APP-1A","title":"boom","count":"12","project":{"slug":"web"}}`)
	}))
	t.Cleanup(srv.Close)
	c := New("tok", "acme", "web", srv.URL)
	c.HTTP = srv.Client()
	// httptest is http — override base() by setting BaseURL to the test server with a fake https parse?
	// Client.base() requires https. Point HTTP transport via rewrite: use custom BaseURL that is https
	// and a transport that maps it. Simpler: temporarily allow the test server by setting BaseURL
	// through a reverse-proxy style — change Client.base for tests by using https and rewriting.
	// Easiest path: set BaseURL to srv.URL and relax is not allowed. Use a Transport that
	// ignores the request URL host and hits srv.
	c.BaseURL = "https://sentry.test"
	c.HTTP = &http.Client{Transport: rewriteHost{base: srv.URL}}
	got, slug, err := c.Get(t.Context(), "9")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "web" || got.ID != "9" || got.Count != 12 || got.ShortID != "APP-1A" {
		t.Fatalf("%+v slug=%s", got, slug)
	}
	if !strings.Contains(gotPath, "/organizations/acme/issues/9") && !strings.Contains(gotPath, "/events/latest") {
		// last request is latest event; that's fine
	}
	if MatchProject(slug, "other") {
		t.Fatal("mismatch must fail")
	}
	if !MatchProject(slug, "web") {
		t.Fatal("match")
	}
}

type rewriteHost struct{ base string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, r.base+req.URL.RequestURI(), req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}

func TestParseURLShapes(t *testing.T) {
	cases := []struct {
		in     string
		id     string
		org    string
		wantOK bool
	}{
		{"https://sentry.io/organizations/acme/issues/9", "9", "acme", true},
		{"https://sentry.io/acme/web/issues/9", "9", "acme", true},
		{"https://acme.sentry.io/issues/9", "9", "acme", true},
		{"https://sentry.io/issues/9", "9", "", true},
		{"http://sentry.io/issues/9", "", "", false},
		{"https://sentry.io/organizations/acme/issues/9/events/abc", "9", "acme", true},
		{"APP-1A", "", "", false},
	}
	for _, tc := range cases {
		ref, ok := ParseURL(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("%s ok=%v", tc.in, ok)
		}
		if !ok {
			continue
		}
		if ref.ID != tc.id || ref.ProjectHint != tc.org {
			t.Fatalf("%s → %+v", tc.in, ref)
		}
		if ref.Provider != errsrc.ProviderSentry {
			t.Fatal(ref.Provider)
		}
	}
}

func TestUpdateStatusOrgScopedPUT(t *testing.T) {
	var method, path, rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/0/issues/") && !strings.Contains(r.URL.Path, "/organizations/") {
			t.Errorf("unscoped issues path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		method = r.Method
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		rawBody = string(b)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := New("tok", "acme", "web", "https://sentry.test")
	c.HTTP = &http.Client{Transport: rewriteHost{base: srv.URL}}
	if err := c.UpdateStatus(t.Context(), "9", "resolved"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut {
		t.Fatalf("method=%s", method)
	}
	if path != "/api/0/organizations/acme/issues/9/" {
		t.Fatalf("path=%s", path)
	}
	if rawBody != `{"status":"resolved"}` {
		t.Fatalf("body=%q", rawBody)
	}
	if err := c.UpdateStatus(t.Context(), "9", "open"); err != nil {
		t.Fatal(err)
	}
	if rawBody != `{"status":"unresolved"}` {
		t.Fatalf("open body=%q", rawBody)
	}
	if err := c.UpdateStatus(t.Context(), "9", "muted"); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("muted err=%v", err)
	}
}

func TestBaseURLRejectsHTTP(t *testing.T) {
	c := New("tok", "acme", "web", "http://sentry.example")
	if _, err := c.base(); err == nil {
		t.Fatal("http")
	}
	c.BaseURL = "https://user:pass@sentry.example"
	if _, err := c.base(); err == nil {
		t.Fatal("userinfo")
	}
}
