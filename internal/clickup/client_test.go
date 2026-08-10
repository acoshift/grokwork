package clickup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetTaskNative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "pk_test" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v2/task/9hx" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("include_markdown_description") != "true" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "9hx", "custom_id": nil, "name": "Fix login",
			"url": "https://app.clickup.com/t/9hx",
			"markdown_description": "body md",
			"status":               map[string]string{"status": "in progress"},
			"list":                 map[string]string{"id": "155"},
			"team_id":              "123",
		})
	}))
	t.Cleanup(srv.Close)

	c := New("pk_test")
	c.Base = srv.URL
	c.HTTP = srv.Client()

	got, err := c.GetTask(context.Background(), "9hx", GetOpts{Markdown: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "9hx" || got.Name != "Fix login" || got.Status != "in progress" {
		t.Fatalf("%+v", got)
	}
	if got.Description != "body md" {
		t.Fatalf("desc=%q", got.Description)
	}
}

func TestGetTaskCustomID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/task/DEV-42" {
			t.Errorf("path=%s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("custom_task_ids") != "true" {
			t.Errorf("custom_task_ids=%q", q.Get("custom_task_ids"))
		}
		if q.Get("team_id") != "999" {
			t.Errorf("team_id=%q", q.Get("team_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "abc1", "custom_id": "DEV-42", "name": "Ship",
			"url": "https://app.clickup.com/t/999/DEV-42",
			"status": map[string]string{"status": "to do"},
			"list":   map[string]string{"id": "1"},
			"team_id": "999",
		})
	}))
	t.Cleanup(srv.Close)

	c := New("k")
	c.Base = srv.URL
	c.HTTP = srv.Client()

	got, err := c.GetTask(context.Background(), "DEV-42", GetOpts{
		CustomTaskIDs: true,
		WorkspaceID:   "999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "abc1" || got.CustomID != "DEV-42" {
		t.Fatalf("%+v", got)
	}
}

func TestGetTaskCustomIDMissingWorkspace(t *testing.T) {
	c := New("k")
	_, err := c.GetTask(context.Background(), "DEV-1", GetOpts{CustomTaskIDs: true})
	if err == nil || !strings.Contains(err.Error(), "workspaceId") {
		t.Fatalf("err=%v", err)
	}
}

func TestListTasksQueryParams(t *testing.T) {
	var sawPath, sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tasks": []map[string]any{
				{"id": "a", "name": "One", "status": map[string]string{"status": "open"}, "url": "https://app.clickup.com/t/a"},
				{"id": "b", "name": "Two", "status": map[string]string{"status": "closed"}, "url": "https://app.clickup.com/t/b"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := New("k")
	c.Base = srv.URL
	c.HTTP = srv.Client()

	tasks, err := c.ListTasks(context.Background(), "15505202", 50)
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v2/list/15505202/task" {
		t.Fatalf("path=%s", sawPath)
	}
	if !strings.Contains(sawQuery, "order_by=updated") {
		t.Fatalf("missing order_by: %s", sawQuery)
	}
	if !strings.Contains(sawQuery, "include_closed=true") {
		t.Fatalf("missing include_closed: %s", sawQuery)
	}
	if len(tasks) != 2 {
		t.Fatalf("len=%d", len(tasks))
	}
}

func TestMissingKey(t *testing.T) {
	_, err := New("").GetTask(context.Background(), "9hx", GetOpts{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLooksLikeCustomID(t *testing.T) {
	if !LooksLikeCustomID("DEV-42") || !LooksLikeCustomID("eng-1") {
		t.Fatal("expected match")
	}
	if LooksLikeCustomID("9hx") || LooksLikeCustomID("nope") || LooksLikeCustomID("DEV-") {
		t.Fatal("expected fail")
	}
}
