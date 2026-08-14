package agentapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/linear"
)

func TestParseLinearRef(t *testing.T) {
	t.Parallel()
	id, err := parseLinearRef("eng-123")
	if err != nil || id != "ENG-123" {
		t.Fatalf("bare: id=%q err=%v", id, err)
	}
	id, err = parseLinearRef("https://linear.app/acme/issue/ENG-99/fix-auth")
	if err != nil || id != "ENG-99" {
		t.Fatalf("url: id=%q err=%v", id, err)
	}
	if _, err := parseLinearRef("  "); err == nil {
		t.Fatal("empty ref")
	}
	if _, err := parseLinearRef("not-a-ticket"); err == nil {
		t.Fatal("junk")
	}
	if _, err := parseLinearRef("see ENG-1 and ENG-2"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("multi: %v", err)
	}
}

func linearFake(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return srv
}

func attachLinear(svc *Service, srv *httptest.Server, teamFor func(string) string) {
	svc.LinearEnabled = func(string) bool { return true }
	svc.LinearAPIKey = func(string) string { return "lin_secret_key" }
	svc.LinearTeamKey = teamFor
	svc.LinearNew = func(key string) *linear.Client {
		c := linear.New(key)
		c.Endpoint = srv.URL
		c.HTTP = srv.Client()
		return c
	}
}

func TestGetLinearIssueIdentifierAndURL(t *testing.T) {
	t.Parallel()
	var teams []string
	srv := linearFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin_secret_key" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if team, _ := req.Variables["team"].(string); team != "" {
			teams = append(teams, team)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{{
						"id": "uuid-1", "identifier": "ENG-123", "title": "Auth",
						"url": "https://linear.app/acme/issue/ENG-123",
						"description": "secret-looking body",
						"state": map[string]string{"name": "Todo"},
						"team":  map[string]string{"key": "ENG"},
					}},
				},
			},
		})
	})
	svc, auth, _, _ := testService(t)
	attachLinear(svc, srv, func(string) string { return "ENG" })
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetLinearIssue(t.Context(), raw, "eng-123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identifier != "ENG-123" || got.Title != "Auth" || got.Description != "secret-looking body" {
		t.Fatalf("%+v", got)
	}
	rawJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawJSON), "lin_secret_key") {
		t.Fatal("API key leaked in tool result")
	}
	got, err = svc.GetLinearIssue(t.Context(), raw, "https://linear.app/acme/issue/ENG-123/slug")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identifier != "ENG-123" {
		t.Fatalf("%+v", got)
	}
	if len(teams) != 2 || teams[0] != "ENG" || teams[1] != "ENG" {
		t.Fatalf("teams=%v", teams)
	}
}

func TestGetLinearIssueDisabledNoKeyForbidden(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := linearFake(t, func(http.ResponseWriter, *http.Request) { hits++ })
	svc, auth, _, _ := testService(t)
	svc.LinearNew = func(key string) *linear.Client {
		c := linear.New(key)
		c.Endpoint = srv.URL
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc.LinearEnabled = func(string) bool { return false }
	svc.LinearAPIKey = func(string) string { return "lin_secret_key" }
	if _, err := svc.GetLinearIssue(t.Context(), raw, "ENG-1"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled: %v", err)
	}
	svc.LinearEnabled = func(string) bool { return true }
	svc.LinearAPIKey = func(string) string { return "" }
	if _, err := svc.GetLinearIssue(t.Context(), raw, "ENG-1"); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("nokey: %v", err)
	}
	caps := agentauth.DefaultShipCaps()
	caps.LinearRead = false
	rawNo, _, err := auth.Mint("t1b", "app", "a", "", caps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc.LinearAPIKey = func(string) string { return "lin_secret_key" }
	if _, err := svc.GetLinearIssue(t.Context(), rawNo, "ENG-1"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("cap: %v", err)
	}
	if hits != 0 {
		t.Fatalf("http hits=%d", hits)
	}
}

func TestListLinearIssuesUsesCredProjectTeam(t *testing.T) {
	t.Parallel()
	var listedFor string
	var body string
	srv := linearFake(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{{
						"id": "u1", "identifier": "ENG-9", "title": "One",
						"url": "https://linear.app/acme/issue/ENG-9",
						"description": "must not appear on list row",
						"state": map[string]string{"name": "Todo"},
						"team":  map[string]string{"key": "ENG"},
					}},
				},
			},
		})
	})
	svc, auth, _, _ := testService(t)
	attachLinear(svc, srv, func(p string) string { listedFor = p; return "ENG" })
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListLinearIssues(t.Context(), raw, 10)
	if err != nil {
		t.Fatal(err)
	}
	if listedFor != "app" {
		t.Fatalf("project=%q", listedFor)
	}
	if len(rows) != 1 || rows[0].Identifier != "ENG-9" || rows[0].Title != "One" {
		t.Fatalf("%+v", rows)
	}
	enc, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "description") || strings.Contains(string(enc), "must not appear") {
		t.Fatalf("description leaked in list JSON: %s", enc)
	}
	if strings.Contains(string(enc), "lin_secret_key") {
		t.Fatal("API key leaked")
	}
	if !strings.Contains(body, "ENG") {
		t.Fatalf("expected team in request: %s", body)
	}

	// Other project's token must not list app's team.
	var otherTeam string
	svc.LinearTeamKey = func(p string) string { otherTeam = p; return "OTH" }
	raw2, _, err := auth.Mint("t2", "other", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListLinearIssues(t.Context(), raw2, 5); err != nil {
		t.Fatal(err)
	}
	if otherTeam != "other" {
		t.Fatalf("wrong-project team lookup=%q", otherTeam)
	}
}

func TestDefaultShipCapsIncludeLinearRead(t *testing.T) {
	t.Parallel()
	if !agentauth.DefaultShipCaps().LinearRead {
		t.Fatal("LinearRead")
	}
}
