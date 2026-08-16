package agentapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/errsrc/sentry"
)

type rewriteHost struct{ base string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, r.base+req.URL.RequestURI(), req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}

func TestGetSentryIssueOrgScopedAndProjectCheck(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/events/latest") {
			io.WriteString(w, `{"entries":[]}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/0/organizations/acme/issues/") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"id":"9","shortId":"APP-1A","title":"boom","count":"12","project":{"slug":"web"}}`)
	}))
	t.Cleanup(srv.Close)

	svc, auth, _, _ := testService(t)
	svc.SentryEnabled = func(string) bool { return true }
	svc.SentryAuthToken = func(string) string { return "tok" }
	svc.SentryOrg = func(string) string { return "acme" }
	svc.SentryProject = func(string) string { return "web" }
	svc.SentryNew = func(token, org, project, base string) *sentry.Client {
		c := sentry.New(token, org, project, "https://sentry.test")
		c.HTTP = &http.Client{Transport: rewriteHost{base: srv.URL}}
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetSentryIssue(t.Context(), raw, "9")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "9" || got.Count != 12 || got.ShortID != "APP-1A" {
		t.Fatalf("%+v", got)
	}
	orgHit := false
	for _, p := range paths {
		if strings.Contains(p, "/api/0/issues/9") && !strings.Contains(p, "/organizations/") {
			t.Fatalf("unscoped get: %s", p)
		}
		if strings.Contains(p, "/organizations/acme/issues/9") {
			orgHit = true
		}
	}
	if !orgHit {
		t.Fatalf("paths=%v", paths)
	}

	svc.SentryProject = func(string) string { return "other" }
	if _, err := svc.GetSentryIssue(t.Context(), raw, "9"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("slug mismatch: %v", err)
	}
}

func TestGetSentryIssueURLOrgMismatchNoFetch(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	svc.SentryEnabled = func(string) bool { return true }
	svc.SentryAuthToken = func(string) string { return "tok" }
	svc.SentryOrg = func(string) string { return "acme" }
	svc.SentryProject = func(string) string { return "web" }
	svc.SentryNew = func(token, org, project, base string) *sentry.Client {
		c := sentry.New(token, org, project, "https://sentry.test")
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetSentryIssue(t.Context(), raw, "https://sentry.io/organizations/other/issues/9")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	if hits != 0 {
		t.Fatalf("fetched: %d", hits)
	}
}
