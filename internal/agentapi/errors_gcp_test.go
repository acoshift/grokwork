package agentapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
)

type staticTok string

func (s staticTok) Token(context.Context) (string, error) { return string(s), nil }

func TestGetGCPErrorGroupStatsAndSamePeriod(t *testing.T) {
	var statsQ, eventsQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/groups/") && !strings.Contains(r.URL.Path, "groupStats") {
			t.Errorf("groups.get: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/groupStats") {
			statsQ = r.URL.RawQuery
			io.WriteString(w, `{"errorGroupStats":[{"group":{"groupId":"g1"},"count":"4","affectedUsersCount":"2","representative":{"message":"boom"}}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			eventsQ = r.URL.RawQuery
			io.WriteString(w, `{"errorEvents":[{"message":"boom\nstack"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	svc, auth, _, _ := testService(t)
	svc.GCPErrorsEnabled = func(string) bool { return true }
	svc.GCPProjectID = func(string) string { return "acme" }
	svc.GCPNew = func(string) *gcperr.Client {
		return &gcperr.Client{ProjectID: "acme", Tokens: staticTok("t"), HTTP: srv.Client(), Endpoint: srv.URL}
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetGCPError(t.Context(), raw, "g1", "PERIOD_1_WEEK")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "g1" || got.Count != 4 {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(statsQ, "groupId=g1") || !strings.Contains(statsQ, "timeRange.period=PERIOD_1_WEEK") {
		t.Fatalf("stats %s", statsQ)
	}
	if !strings.Contains(eventsQ, "timeRange.period=PERIOD_1_WEEK") {
		t.Fatalf("events %s", eventsQ)
	}
}

func TestGetGCPErrorConsoleProjectMismatch(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	svc.GCPErrorsEnabled = func(string) bool { return true }
	svc.GCPProjectID = func(string) string { return "acme" }
	svc.GCPNew = func(string) *gcperr.Client {
		return &gcperr.Client{ProjectID: "acme", Tokens: staticTok("t"), HTTP: srv.Client(), Endpoint: srv.URL}
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetGCPError(t.Context(), raw, "https://console.cloud.google.com/errors/detail/g1?project=other", "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	if hits != 0 {
		t.Fatalf("fetched %d", hits)
	}
}
