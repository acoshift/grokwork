package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

func testServer(t *testing.T) (*Server, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{
		DiscordToken:    "tok",
		DiscordClientID: "424242424242424242",
		AllowedUserIDs:  []string{"u0"},
		AllowedRoleIDs:  []string{},
		Projects:        map[string]string{"proj": proj},
		Channels:        map[string]string{"ch": "proj"},
		AllowedUsers:    map[string]struct{}{"u0": {}},
		AllowedRoles:    map[string]struct{}{},
		GrokBin:         "grok",
		MaxTurns:        40,
		TimeoutMs:       1000,
		HTTPListen:      "127.0.0.1:0",
		ConfigPath:      cfgPath,
		DataDir:         filepath.Join(dir, "data"),
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("thread-99", sessionstore.Entry{
		SessionID: "sess-99",
		Project:   "proj",
		LastUser:  "alice#0",
	}); err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("thread-99", history.Turn{
		User: "alice#0", Prompt: "please fix the flaky test",
		Response: "I fixed it by waiting for the race.", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("thread-99", history.Turn{
		User: "alice#0", Prompt: "ship a PR",
		Response: "Opened https://example.com/pr/1", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}
	b := bot.New(cfg, store, hist)
	return New(cfg, store, hist, b), cfg, dir
}

func TestPagesRender(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	cases := []struct {
		path   string
		marker string
	}{
		{"/", `id="page-dashboard"`},
		{"/history", `id="page-history"`},
		{"/config", `id="page-config"`},
		{"/config", `id="bot-invite"`},
		{"/config", "discord.com/oauth2/authorize"},
		{"/config", "424242424242424242"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.marker) {
				t.Fatalf("missing marker %q in body (len=%d)", tc.marker, len(body))
			}
			if !strings.Contains(body, "Grok Discord") {
				t.Fatal("missing brand")
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "thread-99") || !strings.Contains(body, "alice#0") || !strings.Contains(body, "ship a PR") {
		t.Fatalf("history list missing fields: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/history/thread-99", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := w.Body.String()
	for _, want := range []string{
		`id="page-history-detail"`,
		"please fix the flaky test",
		"I fixed it by waiting for the race.",
		"ship a PR",
		"Opened https://example.com/pr/1",
		"User",
		"Grok",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("history detail missing %q in %s", want, detail)
		}
	}
}

func TestConfigAddsPersist(t *testing.T) {
	srv, cfg, dir := testServer(t)
	h := srv.Handler()

	newProj := filepath.Join(dir, "added-proj")
	if err := os.MkdirAll(newProj, 0o755); err != nil {
		t.Fatal(err)
	}

	type postCase struct {
		path  string
		form  url.Values
		check func(t *testing.T)
	}
	posts := []postCase{
		{
			path: "/config/projects",
			form: url.Values{"name": {"added"}, "path": {newProj}},
			check: func(t *testing.T) {
				p, ok := cfg.ProjectPath("added")
				if !ok || p != newProj {
					t.Fatalf("runtime project: %q %v", p, ok)
				}
			},
		},
		{
			path: "/config/channels",
			form: url.Values{"channelId": {"ch-added"}, "project": {"added"}},
			check: func(t *testing.T) {
				p, ok := cfg.ChannelProject("ch-added")
				if !ok || p != "added" {
					t.Fatalf("runtime channel: %q %v", p, ok)
				}
			},
		},
		{
			path: "/config/users",
			form: url.Values{"id": {"user-added"}},
			check: func(t *testing.T) {
				if !cfg.UserAllowed("user-added") {
					t.Fatal("runtime user missing")
				}
			},
		},
		{
			path: "/config/roles",
			form: url.Values{"id": {"role-added"}},
			check: func(t *testing.T) {
				if !cfg.RoleAllowed("role-added") {
					t.Fatal("runtime role missing")
				}
			},
		},
	}

	for _, p := range posts {
		req := httptest.NewRequest(http.MethodPost, p.path, strings.NewReader(p.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
			t.Fatalf("%s status=%d body=%s", p.path, w.Code, w.Body.String())
		}
		p.check(t)
	}

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"added", newProj, "user-added", "role-added", "ch-added", "Remove", "Add channel map"} {
		if !strings.Contains(body, want) {
			t.Fatalf("config page missing %q", want)
		}
	}

	// Removes
	removes := []postCase{
		{
			path: "/config/users/remove",
			form: url.Values{"id": {"user-added"}},
			check: func(t *testing.T) {
				if cfg.UserAllowed("user-added") {
					t.Fatal("user still allowed")
				}
			},
		},
		{
			path: "/config/roles/remove",
			form: url.Values{"id": {"role-added"}},
			check: func(t *testing.T) {
				if cfg.RoleAllowed("role-added") {
					t.Fatal("role still allowed")
				}
			},
		},
		{
			path: "/config/channels/remove",
			form: url.Values{"channelId": {"ch-added"}},
			check: func(t *testing.T) {
				if _, ok := cfg.ChannelProject("ch-added"); ok {
					t.Fatal("channel still mapped")
				}
			},
		},
		{
			path: "/config/projects/remove",
			form: url.Values{"name": {"added"}},
			check: func(t *testing.T) {
				if _, ok := cfg.ProjectPath("added"); ok {
					t.Fatal("project still present")
				}
			},
		},
	}
	for _, p := range removes {
		req := httptest.NewRequest(http.MethodPost, p.path, strings.NewReader(p.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
			t.Fatalf("%s status=%d body=%s", p.path, w.Code, w.Body.String())
		}
		p.check(t)
	}

	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Projects       map[string]string `json:"projects"`
		Channels       map[string]string `json:"channels"`
		AllowedUserIDs []string          `json:"allowedUserIds"`
		AllowedRoleIDs []string          `json:"allowedRoleIds"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Projects["added"]; ok {
		t.Fatalf("disk still has project: %+v", disk.Projects)
	}
	if _, ok := disk.Channels["ch-added"]; ok {
		t.Fatalf("disk still has channel: %+v", disk.Channels)
	}
	if contains(disk.AllowedUserIDs, "user-added") || contains(disk.AllowedRoleIDs, "role-added") {
		t.Fatalf("disk still has allowlist: %+v %+v", disk.AllowedUserIDs, disk.AllowedRoleIDs)
	}
}

func TestSSE(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = w.Body.String()
		if strings.Contains(body, "data:") {
			cancel()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	var payload string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if payload == "" {
		t.Fatalf("no data event in body: %q", body)
	}
	var snap bot.StatusSnapshot
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		t.Fatalf("unmarshal payload %q: %v", payload, err)
	}
	if snap.SessionCount < 1 {
		t.Fatalf("expected sessionCount>=1 got %+v", snap)
	}
	if snap.ProjectCount < 1 {
		t.Fatalf("expected projectCount>=1 got %+v", snap)
	}
	if snap.Time.IsZero() {
		t.Fatal("time zero in SSE payload")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
