package gcperr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/errsrc"
)

type staticTok string

func (s staticTok) Token(context.Context) (string, error) { return string(s), nil }

func TestGetUsesGroupStatsNotGroupsGet(t *testing.T) {
	var statsQ, eventsQ string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if strings.Contains(r.URL.Path, "/groups/") && !strings.Contains(r.URL.Path, "groupStats") {
			t.Errorf("must not call groups.get: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/groupStats") {
			statsQ = r.URL.RawQuery
			io.WriteString(w, `{"errorGroupStats":[{"group":{"groupId":"g1"},"count":"3","affectedUsersCount":"2","lastSeenTime":"2026-08-01T00:00:00Z","representative":{"message":"panic"}}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			eventsQ = r.URL.RawQuery
			io.WriteString(w, `{"errorEvents":[{"eventTime":"2026-08-01T00:00:00Z","message":"panic:\nstack"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c := &Client{
		ProjectID: "acme",
		Tokens:    staticTok("t"),
		HTTP:      srv.Client(),
		Endpoint:  srv.URL,
	}
	got, err := c.Get(t.Context(), "g1", "PERIOD_1_WEEK")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "g1" || got.Count != 3 || got.UserCount != 2 || !strings.Contains(got.Sample, "panic") {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.URL, "errors/detail/g1") || !strings.Contains(got.URL, "project=acme") {
		t.Fatalf("permalink=%q", got.URL)
	}
	if !strings.Contains(statsQ, "groupId=g1") || !strings.Contains(statsQ, "timeRange.period=PERIOD_1_WEEK") {
		t.Fatalf("stats query %s", statsQ)
	}
	if !strings.Contains(eventsQ, "groupId=g1") || !strings.Contains(eventsQ, "timeRange.period=PERIOD_1_WEEK") {
		t.Fatalf("events query %s", eventsQ)
	}
	if !strings.Contains(eventsQ, "pageSize=5") {
		t.Fatalf("recent pageSize %s", eventsQ)
	}
}

func TestConfiguredPathDoesNotFallThrough(t *testing.T) {
	metaHits := 0
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaHits++
		io.WriteString(w, `{"access_token":"from-meta","expires_in":3600}`)
	}))
	t.Cleanup(meta.Close)
	s := &FileTokenSource{
		CredentialsFile: filepath.Join(t.TempDir(), "missing.json"),
		MetadataURL:     meta.URL,
		HTTP:            meta.Client(),
	}
	if _, err := s.Token(t.Context()); err == nil {
		t.Fatal("unreadable path must error")
	}
	if metaHits != 0 {
		t.Fatalf("fell through to metadata (%d)", metaHits)
	}
}

func TestWellKnownADCUsedWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"from-adc","expires_in":3600}`)
	}))
	t.Cleanup(tokSrv.Close)
	adc := filepath.Join(dir, "application_default_credentials.json")
	body := `{"type":"authorized_user","client_id":"id","client_secret":"sec","refresh_token":"rt","token_uri":"` + tokSrv.URL + `"}`
	if err := os.WriteFile(adc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &FileTokenSource{
		CloudSDKConfig: dir,
		HTTP:           tokSrv.Client(),
		LookupEnv:      func(string) string { return "" },
		MetadataURL:    "http://127.0.0.1:1/nope",
	}
	tok, err := s.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-adc" {
		t.Fatal(tok)
	}
}

func TestParseConsoleURL(t *testing.T) {
	ref, ok := ParseURL("https://console.cloud.google.com/errors/detail/abc123;time=P7D?project=acme-prod")
	if !ok || ref.ID != "abc123" || ref.ProjectHint != "acme-prod" || ref.Provider != errsrc.ProviderGCP {
		t.Fatalf("%v %+v", ok, ref)
	}
	if _, ok := ParseURL("https://console.cloud.google.com/errors"); ok {
		t.Fatal("list page")
	}
	if _, ok := ParseURL("https://console.cloud.google.com/errors/detail/a/b"); ok {
		t.Fatal("slash in id")
	}
}

func TestGroupStatsMapsResolutionStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/groupStats") {
			io.WriteString(w, `{"errorGroupStats":[{"group":{"groupId":"g1","resolutionStatus":"RESOLVED"},"count":"1","representative":{"message":"boom"}}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			io.WriteString(w, `{"errorEvents":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c := &Client{ProjectID: "acme", Tokens: staticTok("t"), HTTP: srv.Client(), Endpoint: srv.URL}
	got, err := c.Get(t.Context(), "g1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" {
		t.Fatalf("status=%q", got.Status)
	}
}

func TestUpdateResolutionGetThenPutKeepsTrackingIssues(t *testing.T) {
	var putBody map[string]any
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, "/projects/acme/groups/g1") {
			t.Errorf("path=%s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `{"name":"projects/acme/groups/g1","groupId":"g1","resolutionStatus":"OPEN","trackingIssues":[{"url":"https://example.com/1"}]}`)
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{ProjectID: "acme", Tokens: staticTok("t"), HTTP: srv.Client(), Endpoint: srv.URL}
	if err := c.UpdateResolution(t.Context(), "g1", "resolved"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != "GET /projects/acme/groups/g1" || methods[1] != "PUT /projects/acme/groups/g1" {
		t.Fatalf("methods=%v", methods)
	}
	if putBody["resolutionStatus"] != "RESOLVED" {
		t.Fatalf("status=%v", putBody["resolutionStatus"])
	}
	issues, ok := putBody["trackingIssues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("trackingIssues=%v", putBody["trackingIssues"])
	}
}

func TestUpdateResolutionRejectsSlashID(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	c := &Client{ProjectID: "acme", Tokens: staticTok("t"), HTTP: srv.Client(), Endpoint: srv.URL}
	if err := c.UpdateResolution(t.Context(), "a/b", "resolved"); err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("err=%v", err)
	}
	if hits != 0 {
		t.Fatalf("hits=%d", hits)
	}
}
