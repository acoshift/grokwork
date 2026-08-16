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
	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func attachDeploys(svc *Service, srv *httptest.Server, project string, loc, name string) {
	svc.DeploysErrorsEnabled = func(string) bool { return true }
	svc.DeploysAPIToken = func(string) string { return "dep-secret" }
	svc.DeploysProject = func(string) string { return project }
	svc.DeploysLocation = func(string) string { return loc }
	svc.DeploysDeployment = func(string) string { return name }
	svc.DeploysNew = func(opts deploys.Options) *deploys.Client {
		c := deploys.New(opts)
		c.Endpoint = srv.URL
		c.HTTP = srv.Client()
		return c
	}
}

func TestGetDeploysErrorMissingLocNameFails(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	attachDeploys(svc, srv, "acme", "", "")
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetDeploysError(t.Context(), raw, "iss_go_nilmap")
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("bare id without defaults: %v", err)
	}
	if hits != 0 {
		t.Fatalf("http hits=%d", hits)
	}
}

func TestGetDeploysErrorCannotNameOtherProject(t *testing.T) {
	t.Parallel()
	hits := 0
	var gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := io.ReadAll(r.Body)
		var req deploys.GetReq
		_ = json.Unmarshal(body, &req)
		gotProject = req.Project
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issue": map[string]any{"id": "iss1", "deployment": "api", "location": "loc", "title": "t"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	attachDeploys(svc, srv, "acme", "loc", "api")
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := "https://console.deploys.app/deployment/errors?project=other&location=loc&name=api&id=iss1"
	if _, err := svc.GetDeploysError(t.Context(), raw, other); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-project url: %v", err)
	}
	if hits != 0 {
		t.Fatalf("must refuse without fetch: hits=%d", hits)
	}
	got, err := svc.GetDeploysError(t.Context(), raw, "loc/api/iss1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "iss1" || gotProject != "acme" {
		t.Fatalf("project=%q got=%+v", gotProject, got)
	}
}

func TestListDeploysErrorsUsesConfiguredProject(t *testing.T) {
	t.Parallel()
	var listed deploys.ListReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &listed)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"issues": []map[string]any{{
					"id": "iss1", "deployment": "api", "location": "gke.x",
					"title": "boom", "status": "open", "count": 3,
					"sampleMessage": "must not appear on list",
				}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	attachDeploys(svc, srv, "acme", "gke.x", "api")
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.ListDeploysErrors(t.Context(), raw, "open", 20, "", "", "gke.x", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if listed.Project != "acme" || listed.Location != "gke.x" || listed.Name != "worker" {
		t.Fatalf("listed=%+v", listed)
	}
	if len(out.Issues) != 1 || out.Issues[0].ID != "iss1" {
		t.Fatalf("%+v", out)
	}
	enc, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "must not appear") || strings.Contains(string(enc), "sample") {
		t.Fatalf("sample leaked in list: %s", enc)
	}
	if strings.Contains(string(enc), "dep-secret") {
		t.Fatal("token leaked")
	}
}

func TestGetDeploysErrorForbiddenAndDisabled(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	attachDeploys(svc, srv, "acme", "loc", "api")
	caps := agentauth.DefaultShipCaps()
	caps.DeploysErrorsRead = false
	raw, _, err := auth.Mint("t1", "app", "a", "", caps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDeploysError(t.Context(), raw, "loc/api/i"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("cap: %v", err)
	}
	raw2, _, err := auth.Mint("t2", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc.DeploysErrorsEnabled = func(string) bool { return false }
	if _, err := svc.GetDeploysError(t.Context(), raw2, "loc/api/i"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled: %v", err)
	}
	if hits != 0 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestErrorSampleCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ä", errsrc.SampleMaxRunes+80)
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
	svc, auth, _, _ := testService(t)
	attachDeploys(svc, srv, "acme", "loc", "api")
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetDeploysError(t.Context(), raw, "i")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sample != errsrc.CapSample(long) {
		t.Fatalf("len=%d", len([]rune(got.Sample)))
	}
}

func TestSessionGetIncludesErrors(t *testing.T) {
	t.Parallel()
	svc, auth, sessions, _ := testService(t)
	e := sessionstore.Entry{Project: "app", Goal: "deploys iss1: boom"}
	if err := e.UpsertError(sessionstore.TrackedError{
		Provider: sessionstore.ErrorProviderDeploys, ID: "iss1",
		Location: "loc", Resource: "api", Title: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Set("t1", e); err != nil {
		t.Fatal(err)
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := svc.SessionGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Errors) != 1 || info.Errors[0].ID != "iss1" {
		t.Fatalf("%+v", info.Errors)
	}
}

func TestParseDeploysRef(t *testing.T) {
	t.Parallel()
	loc, name, id, hint, err := parseDeploysRef("gke.x/api/iss1", "", "")
	if err != nil || loc != "gke.x" || name != "api" || id != "iss1" || hint != "" {
		t.Fatalf("%s %s %s %s %v", loc, name, id, hint, err)
	}
	_, _, _, _, err = parseDeploysRef("iss1", "", "")
	if err == nil {
		t.Fatal("bare id without defaults")
	}
	loc, name, id, _, err = parseDeploysRef("iss1", "def-loc", "def-api")
	if err != nil || loc != "def-loc" || name != "def-api" || id != "iss1" {
		t.Fatalf("defaults: %s %s %s %v", loc, name, id, err)
	}
}
