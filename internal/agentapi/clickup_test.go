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
	"github.com/acoshift/grokwork/internal/clickup"
)

func TestParseClickUpRef(t *testing.T) {
	t.Parallel()
	native, custom, ws, err := parseClickUpRef("https://app.clickup.com/t/9hx")
	if err != nil || native != "9hx" || custom != "" {
		t.Fatalf("native url: n=%q c=%q err=%v", native, custom, err)
	}
	native, custom, ws, err = parseClickUpRef("https://app.clickup.com/t/1234567/DEV-9")
	if err != nil || custom != "DEV-9" || ws != "1234567" || native != "" {
		t.Fatalf("custom url: n=%q c=%q ws=%q err=%v", native, custom, ws, err)
	}
	// Bare custom id without configured prefix (2br-api shape).
	native, custom, ws, err = parseClickUpRef("DEV-12")
	if err != nil || custom != "DEV-12" || native != "" || ws != "" {
		t.Fatalf("bare custom: n=%q c=%q ws=%q err=%v", native, custom, ws, err)
	}
	native, custom, _, err = parseClickUpRef("9hxabc")
	if err != nil || native != "9hxabc" || custom != "" {
		t.Fatalf("native: n=%q c=%q err=%v", native, custom, err)
	}
	if _, _, _, err := parseClickUpRef("  "); err == nil {
		t.Fatal("empty ref")
	}
}

func TestGetClickUpTaskCustomIDUsesWorkspace(t *testing.T) {
	t.Parallel()
	var sawCustom, sawTeam bool
	var gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("custom_task_ids") == "true" {
			sawCustom = true
		}
		if r.URL.Query().Get("team_id") == "ws1" {
			sawTeam = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "abc", "custom_id": "DEV-12", "name": "Auth",
			"url": "https://app.clickup.com/t/abc",
			"status": map[string]string{"status": "open"},
			"markdown_description": "body",
		})
	}))
	t.Cleanup(srv.Close)

	svc, auth, _, _ := testService(t)
	svc.ClickUpEnabled = func(p string) bool { gotProject = p; return true }
	svc.ClickUpAPIKey = func(string) string { return "pk_test" }
	svc.ClickUpWorkspaceID = func(string) string { return "ws1" }
	svc.ClickUpNew = func(key string) *clickup.Client {
		c := clickup.New(key)
		c.Base = srv.URL
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetClickUpTask(t.Context(), raw, "DEV-12")
	if err != nil {
		t.Fatal(err)
	}
	if gotProject != "app" {
		t.Fatalf("project=%q", gotProject)
	}
	if !sawCustom || !sawTeam {
		t.Fatalf("expected custom_task_ids+team_id; custom=%v team=%v", sawCustom, sawTeam)
	}
	if got.ID != "abc" || got.CustomID != "DEV-12" || got.Name != "Auth" || got.Description != "body" {
		t.Fatalf("%+v", got)
	}
}

func TestGetClickUpTaskNativeNoCustomQuery(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("custom_task_ids") != "" {
			t.Errorf("native id must not set custom_task_ids: %s", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.Path, "/9hx") {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "9hx", "name": "N", "url": "https://app.clickup.com/t/9hx",
			"status": map[string]string{"status": "open"},
		})
	}))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	svc.ClickUpEnabled = func(string) bool { return true }
	svc.ClickUpAPIKey = func(string) string { return "pk" }
	svc.ClickUpNew = func(key string) *clickup.Client {
		c := clickup.New(key)
		c.Base = srv.URL
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetClickUpTask(t.Context(), raw, "9hx")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "9hx" {
		t.Fatalf("%+v", got)
	}
}

func TestGetClickUpTaskDisabledAndNoKey(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	svc.ClickUpNew = func(key string) *clickup.Client {
		c := clickup.New(key)
		c.Base = srv.URL
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc.ClickUpEnabled = func(string) bool { return false }
	svc.ClickUpAPIKey = func(string) string { return "pk" }
	if _, err := svc.GetClickUpTask(t.Context(), raw, "DEV-1"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled: %v", err)
	}
	svc.ClickUpEnabled = func(string) bool { return true }
	svc.ClickUpAPIKey = func(string) string { return "" }
	if _, err := svc.GetClickUpTask(t.Context(), raw, "DEV-1"); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("nokey: %v", err)
	}
	if hits != 0 {
		t.Fatalf("http hits=%d", hits)
	}
}

func TestGetClickUpTaskForbiddenWithoutCap(t *testing.T) {
	t.Parallel()
	svc, auth, _, _ := testService(t)
	caps := agentauth.DefaultShipCaps()
	caps.ClickUpRead = false
	raw, _, err := auth.Mint("t1", "app", "a", "", caps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetClickUpTask(t.Context(), raw, "DEV-1"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("%v", err)
	}
}

func TestListClickUpTasksUsesCredProjectList(t *testing.T) {
	t.Parallel()
	var listPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		listPath = r.URL.Path
		io.WriteString(w, `{"tasks":[{"id":"a","name":"One","status":{"status":"open"},"url":"https://app.clickup.com/t/a"}]}`)
	}))
	t.Cleanup(srv.Close)
	svc, auth, _, _ := testService(t)
	var listedFor string
	svc.ClickUpEnabled = func(string) bool { return true }
	svc.ClickUpAPIKey = func(string) string { return "pk" }
	svc.ClickUpListID = func(p string) string { listedFor = p; return "list-9" }
	svc.ClickUpNew = func(key string) *clickup.Client {
		c := clickup.New(key)
		c.Base = srv.URL
		c.HTTP = srv.Client()
		return c
	}
	raw, _, err := auth.Mint("t1", "app", "a", "", agentauth.DefaultShipCaps(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListClickUpTasks(t.Context(), raw, 10)
	if err != nil {
		t.Fatal(err)
	}
	if listedFor != "app" || !strings.Contains(listPath, "/list/list-9/") {
		t.Fatalf("project=%q path=%s", listedFor, listPath)
	}
	if len(rows) != 1 || rows[0].ID != "a" || rows[0].Name != "One" {
		t.Fatalf("%+v", rows)
	}
}

func TestDefaultShipCapsIncludeClickUpRead(t *testing.T) {
	t.Parallel()
	if !agentauth.DefaultShipCaps().ClickUpRead {
		t.Fatal("ClickUpRead")
	}
}
