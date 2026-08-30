package deploys

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
)

func TestListAndGetHappyPath(t *testing.T) {
	t.Parallel()
	var sawChannel, sawAuth, sawAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawChannel = r.Header.Get(headerChannel)
		sawAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		action := strings.TrimPrefix(r.URL.Path, "/")
		sawAction = action
		body, _ := io.ReadAll(r.Body)
		switch action {
		case "error.list":
			var req ListReq
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if req.Project != "acme" || req.Location != "" || req.Name != "" {
				t.Fatalf("list req=%+v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"issues": []map[string]any{{
						"id": "iss_go_nilmap", "deployment": "api", "location": "gke.cluster-rcf2",
						"fingerprint": "fp1", "kind": "go", "title": "nil map",
						"status": "open", "count": 12,
						"firstSeen": time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
						"lastSeen":  time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
					}},
					"nextCursor": "c2",
				},
			})
		case "error.get":
			var req GetReq
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if req.Project != "acme" || req.Location != "gke.cluster-rcf2" || req.Name != "api" || req.ID != "iss_go_nilmap" {
				t.Fatalf("get req=%+v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"issue": map[string]any{
						"id": "iss_go_nilmap", "deployment": "api", "location": "gke.cluster-rcf2",
						"kind": "go", "title": "nil map", "status": "open", "count": 12,
						"sampleMessage": "panic: assignment to entry in nil map",
						"recentEvents": []map[string]any{{
							"pod": "api-1", "timestamp": time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC),
							"object": "logs/a", "offset": 9,
						}},
					},
				},
			})
		default:
			t.Errorf("action=%s", action)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Options{Token: "tok-secret", Endpoint: srv.URL, HTTP: srv.Client()})
	list, err := c.List(t.Context(), ListReq{Project: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if sawChannel != channelValue {
		t.Fatalf("channel=%q", sawChannel)
	}
	if sawAuth != "Bearer tok-secret" {
		t.Fatalf("auth=%q", sawAuth)
	}
	if sawAction != "error.list" {
		t.Fatalf("action=%q", sawAction)
	}
	if list.NextCursor != "c2" || len(list.Groups) != 1 {
		t.Fatalf("%+v", list)
	}
	g := list.Groups[0]
	if g.Provider != errsrc.ProviderDeploys || g.ID != "iss_go_nilmap" || g.Location != "gke.cluster-rcf2" || g.Resource != "api" {
		t.Fatalf("%+v", g)
	}
	if !strings.Contains(g.URL, "location=gke.cluster-rcf2") || !strings.Contains(g.URL, "name=api") {
		t.Fatalf("url=%q", g.URL)
	}

	got, err := c.Get(t.Context(), GetReq{Project: "acme", Location: "gke.cluster-rcf2", Name: "api", ID: "iss_go_nilmap"})
	if err != nil {
		t.Fatal(err)
	}
	if sawAction != "error.get" {
		t.Fatalf("action=%q", sawAction)
	}
	if got.Sample != "panic: assignment to entry in nil map" {
		t.Fatalf("sample=%q", got.Sample)
	}
	if len(got.Recent) != 1 || got.Recent[0].Culprit != "api-1" || got.Recent[0].Extra != "logs/a:9" {
		t.Fatalf("recent=%+v", got.Recent)
	}
	if got.Recent[0].Timestamp.IsZero() {
		t.Fatal("recent timestamp empty — ErrorOccurrence uses json:timestamp")
	}
}

func TestUpdateHappyPath(t *testing.T) {
	t.Parallel()
	var sawAction, sawAuth, sawChannel string
	var got UpdateReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawChannel = r.Header.Get(headerChannel)
		sawAuth = r.Header.Get("Authorization")
		sawAction = strings.TrimPrefix(r.URL.Path, "/")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Token: "tok-secret", Endpoint: srv.URL, HTTP: srv.Client()})
	if err := c.Update(t.Context(), UpdateReq{
		Project: "acme", Location: "gke.cluster-rcf2", Name: "api", ID: "iss_go_nilmap", Status: "resolved",
	}); err != nil {
		t.Fatal(err)
	}
	if sawAction != "error.update" {
		t.Fatalf("action=%q", sawAction)
	}
	if sawAuth != "Bearer tok-secret" {
		t.Fatalf("auth=%q", sawAuth)
	}
	if sawChannel != channelValue {
		t.Fatalf("channel=%q", sawChannel)
	}
	if got.Project != "acme" || got.Location != "gke.cluster-rcf2" || got.Name != "api" || got.ID != "iss_go_nilmap" || got.Status != "resolved" {
		t.Fatalf("%+v", got)
	}
}

func TestUpdateMissingLocatorFailsLocally(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	c := New(Options{Token: "tok", Endpoint: srv.URL, HTTP: srv.Client()})
	if err := c.Update(t.Context(), UpdateReq{Project: "acme", ID: "iss1", Status: "resolved"}); err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("err=%v", err)
	}
	if err := c.Update(t.Context(), UpdateReq{Project: "acme", Location: "loc", Name: "api", ID: "iss1", Status: "muted"}); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("muted err=%v", err)
	}
	if hits != 0 {
		t.Fatalf("http hits=%d", hits)
	}
}

func TestGetWithoutLocationOrNameFailsLocally(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	c := New(Options{Token: "tok", Endpoint: srv.URL, HTTP: srv.Client()})
	_, err := c.Get(t.Context(), GetReq{Project: "acme", ID: "iss1"})
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("err=%v", err)
	}
	_, err = c.Get(t.Context(), GetReq{Project: "acme", Location: "loc", ID: "iss1"})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("err=%v", err)
	}
	if hits != 0 {
		t.Fatalf("http hits=%d — must not substitute defaults", hits)
	}
}

func TestAuthHeaderBasicVsBearer(t *testing.T) {
	t.Parallel()
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get(headerChannel) != channelValue {
			t.Errorf("channel=%q", r.Header.Get(headerChannel))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"issues": []any{}},
		})
	}))
	t.Cleanup(srv.Close)

	bearer := New(Options{Token: "abc", Endpoint: srv.URL, HTTP: srv.Client()})
	if _, err := bearer.List(t.Context(), ListReq{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	basic := New(Options{Token: "ignored", BasicUser: "sa", BasicPass: "secret", Endpoint: srv.URL, HTTP: srv.Client()})
	if _, err := basic.List(t.Context(), ListReq{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 || auths[0] != "Bearer abc" {
		t.Fatalf("auths=%v", auths)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("sa:secret"))
	if auths[1] != wantBasic {
		t.Fatalf("basic=%q want %q", auths[1], wantBasic)
	}
}

func TestChannelHeaderSet(t *testing.T) {
	t.Parallel()
	var channel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channel = r.Header.Get("X-Deploys-Channel")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"issues": []any{}}})
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Token: "t", Endpoint: srv.URL, HTTP: srv.Client()})
	if _, err := c.List(t.Context(), ListReq{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if channel != "grokwork" {
		t.Fatalf("channel=%q", channel)
	}
}

func TestGetCapsSample(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", errsrc.SampleMaxRunes+50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issue": map[string]any{
					"id": "i", "deployment": "api", "location": "loc",
					"title": "t", "sampleMessage": long,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Token: "t", Endpoint: srv.URL, HTTP: srv.Client()})
	got, err := c.Get(t.Context(), GetReq{Project: "p", Location: "loc", Name: "api", ID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sample != errsrc.CapSample(long) {
		t.Fatalf("len=%d", len(got.Sample))
	}
}
