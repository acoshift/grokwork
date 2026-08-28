package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

func TestHomePageCountsStayMounted(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/")
	start := strings.Index(body, `id="live-home-projects"`)
	if start < 0 {
		t.Fatal(`missing id="live-home-projects"`)
	}
	runs := strings.Index(body, `id="live-home-runs"`)
	if runs < 0 || runs <= start {
		t.Fatal(`missing id="live-home-runs"`)
	}
	host := body[start:runs]
	for _, want := range []string{
		`hx-swap="none"`,
		`/partials/home/counts`,
		`data-home-counts`,
		`data-project="proj"`,
		`data-home-stat="running"`,
		`data-home-stat="queued"`,
		`data-home-stat="openPRs"`,
		`data-home-stat="checksFailing"`,
		`data-home-stat="idle"`,
	} {
		if !strings.Contains(host, want) {
			t.Fatalf("home projects host missing %q", want)
		}
	}
	if strings.Contains(host, `hx-swap="innerHTML"`) {
		t.Fatal("home project cards must not innerHTML-swap")
	}
	if strings.Contains(host, `/partials/home/projects`) {
		t.Fatal("home live path must be counts JSON, not projects HTML")
	}
	runHost := body[runs:]
	if i := strings.Index(runHost, `hx-swap="innerHTML"`); i < 0 || i > 400 {
		t.Fatal("home runs must stay an innerHTML live-region")
	}
	if !strings.Contains(body, "__gwHomeCountsBound") {
		t.Fatal("home page missing counts paint script")
	}
}

func TestHomeCountsJSON(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/partials/home/counts", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control=%q want no-store", cc)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type=%q want JSON", ct)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	proj, ok := raw["proj"]
	if !ok {
		t.Fatalf("missing proj: %v", raw)
	}
	for _, key := range []string{"running", "queued", "openPRs", "checksFailing"} {
		v, ok := proj[key]
		if !ok {
			t.Fatalf("missing %s (omitzero?): %v", key, proj)
		}
		n, ok := v.(float64)
		if !ok {
			t.Fatalf("%s = %T %v", key, v, v)
		}
		if n != 0 {
			t.Fatalf("%s=%v want 0", key, n)
		}
	}
}

func TestHomeCountsHidesUnauthorizedProject(t *testing.T) {
	srv := twoProjectAuthServer(t)
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	req := httptest.NewRequest(http.MethodGet, "/partials/home/counts", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got map[string]homeProjectCount
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	if _, ok := got["secret"]; ok {
		t.Fatalf("member saw secret project: %v", got)
	}
	pub, ok := got["public"]
	if !ok {
		t.Fatalf("member missing public: %v", got)
	}
	if pub.OpenPRs != 1 {
		t.Fatalf("public openPRs=%d want 1: %v", pub.OpenPRs, got)
	}
}

func TestOverviewPulseCountsStayMounted(t *testing.T) {
	srv, _, _ := testServer(t)
	body := getBody(t, srv.Handler(), "/projects/proj")
	start := strings.Index(body, `id="live-project-pulse"`)
	if start < 0 {
		t.Fatal(`missing id="live-project-pulse"`)
	}
	runs := strings.Index(body, `id="live-project-runs"`)
	if runs < 0 || runs <= start {
		t.Fatal(`missing id="live-project-runs"`)
	}
	host := body[start:runs]
	for _, want := range []string{
		`hx-swap="none"`,
		`/partials/projects/pulse/counts`,
		`data-pulse-counts`,
		`data-pulse="running"`,
		`data-pulse="queued"`,
		`data-pulse="openPRs"`,
		`data-pulse="checksFailing"`,
		`data-pulse="casesOpen"`,
		`data-pulse="investigate"`,
		`data-pulse="engineering"`,
		`id="pulse-running"`,
	} {
		if !strings.Contains(host, want) {
			t.Fatalf("pulse numbers host missing %q", want)
		}
	}
	if strings.Contains(host, `hx-swap="innerHTML"`) {
		t.Fatal("pulse number cards must not innerHTML-swap")
	}
	runHost := body[runs:]
	end := strings.Index(runHost, `id="m-workspace-browse"`)
	if end < 0 {
		end = min(len(runHost), 800)
	}
	runsHTML := runHost[:end]
	if !strings.Contains(runsHTML, `hx-swap="innerHTML"`) {
		t.Fatal("project runs must stay an innerHTML live-region")
	}
	if !strings.Contains(runsHTML, `/partials/projects/pulse/runs`) {
		t.Fatal("project runs missing runs fragment URL")
	}
	if !strings.Contains(body, "__gwPulseCountsBound") {
		t.Fatal("overview missing pulse counts paint script")
	}
}

func TestPulseCountsJSON(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/partials/projects/pulse/counts?project=proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control=%q want no-store", cc)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	for _, key := range []string{
		"running", "queued", "openPRs", "checksFailing",
		"casesOpen", "investigate", "engineering",
		"draft", "closed", "intake", "answered", "fixing", "shipping",
	} {
		v, ok := raw[key]
		if !ok {
			t.Fatalf("missing %s (omitzero?): %v", key, raw)
		}
		if _, ok := v.(float64); !ok {
			t.Fatalf("%s = %T %v", key, v, v)
		}
	}
}

func TestPulseCountsForbidden(t *testing.T) {
	srv := twoProjectAuthServer(t)
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}

	req := httptest.NewRequest(http.MethodGet, "/partials/projects/pulse/counts?project=secret", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("secret pulse counts status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/partials/projects/pulse/counts?project=public", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("public pulse counts status=%d body=%s", w.Code, w.Body.String())
	}
	var got projectPulseCounts
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	if got.OpenPRs != 1 {
		t.Fatalf("public openPRs=%d want 1", got.OpenPRs)
	}
}
