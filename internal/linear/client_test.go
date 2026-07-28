package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetByIdentifier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Variables["team"] != "ENG" {
			t.Errorf("team=%v", req.Variables["team"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":          "uuid-1",
							"identifier":  "ENG-123",
							"title":       "Fix auth",
							"url":         "https://linear.app/acme/issue/ENG-123",
							"description": "Details here",
							"state":       map[string]string{"name": "In Progress"},
							"team":        map[string]string{"key": "ENG"},
						},
					},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := New("test-key")
	c.Endpoint = srv.URL
	c.HTTP = srv.Client()

	iss, err := c.GetByIdentifier(context.Background(), "eng-123")
	if err != nil {
		t.Fatal(err)
	}
	if iss.ID != "uuid-1" || iss.Identifier != "ENG-123" || iss.State != "In Progress" {
		t.Fatalf("%+v", iss)
	}
}

func TestGetByIdentifierNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"issues": map[string]any{"nodes": []any{}}},
		})
	}))
	t.Cleanup(srv.Close)

	c := New("k")
	c.Endpoint = srv.URL
	c.HTTP = srv.Client()
	_, err := c.GetByIdentifier(context.Background(), "ENG-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSplitIdentifier(t *testing.T) {
	team, n, ok := splitIdentifier("eng-42")
	if !ok || team != "ENG" || n != 42 {
		t.Fatalf("%s %d %v", team, n, ok)
	}
	if _, _, ok := splitIdentifier("nope"); ok {
		t.Fatal("expected fail")
	}
}

func TestMissingKey(t *testing.T) {
	_, err := New("").GetByIdentifier(context.Background(), "ENG-1")
	if err == nil {
		t.Fatal("expected error")
	}
}
