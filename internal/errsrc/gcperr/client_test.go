package gcperr

import (
	"context"
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
