package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// TestLiveHTTPLaunch boots the production web entry (hime ListenAndServe path
// via Serve on a real TCP listener) and exercises pages, config POSTs, and SSE.
func TestLiveHTTPLaunch(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{
		DiscordToken: "tok",
		Projects: config.ProjectsMap{
			"proj": {Path: proj, AllowedUserIDs: []string{"u0"}},
		},
		Channels: map[string]string{"ch": "proj"},
		GrokBin:  "grok",
		MaxTurns:       40,
		TimeoutMs:      1000,
		HTTPListen:     "127.0.0.1:0",
		ConfigPath:     cfgPath,
		DataDir:        filepath.Join(dir, "data"),
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("th-live", sessionstore.Entry{
		SessionID: "sid-live",
		Project:   "proj",
		LastUser:  "bob",
	}); err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Append("th-live", history.Turn{
		User: "bob", Prompt: "live prompt", Response: "live reply",
		Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, store, hist, bot.New(cfg, store, hist))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.App().Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Shutdown()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	})

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}

	// Pages
	for _, path := range []string{"/", "/ship", "/history", "/config"} {
		res, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, res.StatusCode, body)
		}
		text := string(body)
		switch path {
		case "/":
			if !strings.Contains(text, `id="page-home"`) {
				t.Fatalf("home launcher marker missing")
			}
		case "/ship":
			if !strings.Contains(text, `id="page-ship"`) {
				t.Fatalf("ship board marker missing: %s", text)
			}
		case "/history":
			if !strings.Contains(text, `id="page-history"`) || !strings.Contains(text, "th-live") {
				t.Fatalf("history marker/session missing: %s", text)
			}
			// Detail page with per-turn messages.
			dres, err := client.Get(base + "/history/th-live")
			if err != nil {
				t.Fatalf("GET detail: %v", err)
			}
			dbody, _ := io.ReadAll(dres.Body)
			dres.Body.Close()
			if dres.StatusCode != http.StatusOK {
				t.Fatalf("detail status=%d", dres.StatusCode)
			}
			dtext := string(dbody)
			if !strings.Contains(dtext, "live prompt") || !strings.Contains(dtext, "live reply") {
				t.Fatalf("detail missing turns: %s", dtext)
			}
		case "/config":
			if !strings.Contains(text, `id="page-config"`) {
				t.Fatalf("config marker missing")
			}
		}
	}

	// Config adds
	newProj := filepath.Join(dir, "live-added")
	if err := os.MkdirAll(newProj, 0o755); err != nil {
		t.Fatal(err)
	}
	posts := []struct {
		path string
		form url.Values
	}{
		{"/config/projects", url.Values{"name": {"liveproj"}, "path": {newProj}}},
		{"/config/projects/users", url.Values{"name": {"proj"}, "id": {"live-user"}}},
		{"/config/projects/roles", url.Values{"name": {"proj"}, "id": {"live-role"}}},
	}
	for _, p := range posts {
		res, err := client.PostForm(base+p.path, p.form)
		if err != nil {
			t.Fatalf("POST %s: %v", p.path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound {
			t.Fatalf("POST %s status=%d body=%s", p.path, res.StatusCode, body)
		}
	}

	if p, ok := cfg.ProjectPath("liveproj"); !ok || p != newProj {
		t.Fatalf("runtime project missing: %q %v", p, ok)
	}
	if !cfg.AccessAllowed("proj", "live-user", nil) || !cfg.AccessAllowed("proj", "x", []string{"live-role"}) {
		t.Fatal("runtime project members missing after POST")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "liveproj") || !strings.Contains(string(raw), "live-user") || !strings.Contains(string(raw), "live-role") {
		t.Fatalf("config file missing adds: %s", raw)
	}

	// SSE: read first event
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type=%q", ct)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", res.StatusCode)
	}
	reader := bufio.NewReader(res.Body)
	var payload string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if payload == "" {
		t.Fatal("no SSE data event")
	}
	var snap bot.StatusSnapshot
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		t.Fatalf("SSE json: %v payload=%s", err, payload)
	}
	if snap.SessionCount < 1 || snap.ProjectCount < 1 {
		t.Fatalf("SSE snapshot weak: %+v", snap)
	}
	t.Logf("live launch ok on %s snapshot active=%d sessions=%d projects=%d", addr, snap.ActiveCount, snap.SessionCount, snap.ProjectCount)
	fmt.Fprintf(os.Stderr, "live-http ok addr=%s\n", addr)
}
