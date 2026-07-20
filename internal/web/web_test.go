package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
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
		Projects: config.ProjectsMap{
			"proj": {Path: proj, AllowedUserIDs: []string{"u0"}},
		},
		Channels:        map[string]string{"ch": "proj"},
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
	if err := hist.Append("thread-99", history.Turn{
		User: "alice#0", Prompt: "do a huge refactor",
		Response: "Working…", Status: "error", ExitCode: 1,
		Error: "Reached max turns before a final reply", Project: "proj",
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
		{"/ship", `id="page-ship"`},
		{"/sessions", `id="page-sessions"`},
		{"/history", `id="page-history"`},
		{"/worktrees", `id="page-worktrees"`},
		{"/worktrees", "Prune idle now"},
		{"/config", `id="page-config"`},
		{"/config", `id="bot-invite"`},
		{"/config", "discord.com/oauth2/authorize"},
		{"/config", "Pin Messages"},
		{"/config", "Open / re-authorize"},
		{"/config", "424242424242424242"},
		{"/config", "Default Discord guild"},
		{"/config", `href="/config/projects/proj"`},
		// Per-project settings page owns GitHub/Discord/Linear forms.
		{"/config/projects/proj", `id="page-project-config"`},
		{"/config/projects/proj", "Discord guild ID"},
		{"/config/projects/proj", "name=\"guildId\""},
		{"/config/projects/proj", "GitHub repositories"},
		{"/config/projects/proj", "LINEAR_API_KEY_PROJ"},
		{"/config/projects/proj", "Danger zone"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
				t.Fatalf("Cache-Control=%q want no-store", cc)
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.marker) {
				t.Fatalf("missing marker %q in body (len=%d)", tc.marker, len(body))
			}
			if !strings.Contains(body, "Grok Work") {
				t.Fatal("missing Grok Work brand")
			}
			if strings.Contains(body, "Grok Discord") {
				t.Fatal("legacy Grok Discord brand still present in chrome")
			}
			// Layout hosts SSE; pages host domain live-regions.
			// Shell is hx-boosted into #live-root so the SSE socket survives in-app nav.
			for _, live := range []string{
				`id="live-root"`,
				`hx-history-elt`,
				`id="sse-status"`,
				`hx-ext="sse"`,
				`sse-connect=`,
				"/events",
				"/static/htmx.min.js",
				"live-region",
				`hx-boost="true"`,
				`hx-target="#live-root"`,
				`hx-select="#live-root"`,
				`hx-swap="outerHTML show:none focus-scroll:false"`,
				`hx-inherit="*"`,
				// Config is set from htmx script onload (inline defer is a no-op without src).
				`disableInheritance=true`,
				`scrollIntoViewOnBoost=false`,
				`onload=`,
				`boostScrollByPath`,
			} {
				if !strings.Contains(body, live) {
					t.Fatalf("path %s missing live marker %q", tc.path, live)
				}
			}
		})
	}

	// Domain partials return content-only HTML via hime View("page#define")
	// (no layout/nav/scripts). Live admin data is Cache-Control: no-store.
	t.Run("domain partials", func(t *testing.T) {
		paths := []struct {
			path   string
			marker string
			domain string // hx-trigger domain expected on full page, empty for partial-only
		}{
			{"/partials/dashboard/stats", `id="stats"`, "dashboard"},
			{"/partials/dashboard/runs", `id="runs-wrap"`, "dashboard"},
			{"/partials/ship/stats", "CI failing", "ship"},
			{"/partials/ship/table", "Pull requests", "ship"},
			{"/partials/history/table", "thread-99", "history"},
			{"/partials/history/turns/thread-99", `id="turns"`, "history"},
			{"/partials/worktrees/table", "All worktrees", "worktrees"},
			{"/partials/config/lists", "Projects", "config"},
		}
		for _, tc := range paths {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			// SSE/htmx live regions send HX-Request; fragments stay content-only either way.
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", tc.path, w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.marker) {
				t.Fatalf("%s missing marker %q body=%s", tc.path, tc.marker, body)
			}
			if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
				t.Fatalf("%s Cache-Control=%q want no-store", tc.path, cc)
			}
			for _, ban := range []string{
				`id="sse-status"`,
				"<nav>",
				"/static/htmx.min.js",
				`hx-ext="sse"`,
			} {
				if strings.Contains(body, ban) {
					t.Fatalf("%s partial should not include %q (got full page?)", tc.path, ban)
				}
			}
		}

		// Full pages bind the right domain events.
		pageDomains := []struct {
			path   string
			events []string
		}{
			{"/", []string{`hx-trigger="sse:dashboard"`}},
			{"/ship", []string{`hx-trigger="sse:ship"`}},
			{"/history", []string{`hx-trigger="sse:history"`}},
			{"/history/thread-99", []string{`hx-trigger="sse:history"`}},
			{"/worktrees", []string{`hx-trigger="sse:worktrees"`}},
			{"/config", []string{`hx-trigger="sse:config"`}},
		}
		for _, tc := range pageDomains {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			body := w.Body.String()
			for _, ev := range tc.events {
				if !strings.Contains(body, ev) {
					t.Fatalf("%s missing %q", tc.path, ev)
				}
			}
			// Live regions must not inherit shell hx-target/hx-select (#live-root),
			// or SSE partials wipe the whole page (partial HTML has no #live-root).
			if strings.Count(body, `class="live-region"`) == 0 {
				t.Fatalf("%s: expected live-region", tc.path)
			}
			if !strings.Contains(body, `hx-target="this"`) || !strings.Contains(body, `hx-select="unset"`) {
				t.Fatalf("%s: live-region must set hx-target=this and hx-select=unset", tc.path)
			}
		}
	})

	t.Run("static assets", func(t *testing.T) {
		for _, path := range []string{"/static/htmx.min.js", "/static/sse.js"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("%s status=%d", path, w.Code)
			}
			if w.Body.Len() < 100 {
				t.Fatalf("%s body too small: %d", path, w.Body.Len())
			}
		}
	})

	t.Run("pwa install assets", func(t *testing.T) {
		// Manifest + SW are public (no auth) so browsers can install before login.
		req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("manifest status=%d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/manifest+json") {
			t.Fatalf("manifest Content-Type=%q", ct)
		}
		body := w.Body.String()
		for _, want := range []string{`"name": "Grok Work"`, `"display": "standalone"`, `/static/icon-192.png`, `/static/icon-512.png`} {
			if !strings.Contains(body, want) {
				t.Fatalf("manifest missing %q", want)
			}
		}

		req = httptest.NewRequest(http.MethodGet, "/sw.js", nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("sw status=%d", w.Code)
		}
		if w.Header().Get("Service-Worker-Allowed") != "/" {
			t.Fatalf("Service-Worker-Allowed=%q want /", w.Header().Get("Service-Worker-Allowed"))
		}
		if !strings.Contains(w.Body.String(), "fetch") {
			t.Fatal("sw must register a fetch handler for installability")
		}

		for _, path := range []string{
			"/static/icon-192.png",
			"/static/icon-512.png",
			"/static/icon-maskable-512.png",
			"/static/apple-touch-icon.png",
			"/static/favicon.svg",
			"/static/logo.svg",
		} {
			req = httptest.NewRequest(http.MethodGet, path, nil)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("%s status=%d", path, w.Code)
			}
			if w.Body.Len() < 20 {
				t.Fatalf("%s body too small: %d", path, w.Body.Len())
			}
		}

		// Pages advertise install metadata.
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		page := w.Body.String()
		for _, want := range []string{
			`rel="manifest"`,
			`/manifest.webmanifest`,
			`apple-touch-icon`,
			`/static/apple-touch-icon.png`,
			`serviceWorker.register("/sw.js")`,
			`apple-mobile-web-app-capable`,
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("page missing PWA marker %q", want)
			}
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "thread-99") || !strings.Contains(body, "alice#0") || !strings.Contains(body, "do a huge refactor") {
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
		"do a huge refactor",
		"Reached max turns before a final reply",
		`class="turn-error"`,
		"exit 1",
		"User",
		"Grok",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("history detail missing %q in %s", want, detail)
		}
	}
}

func TestSessionsHub(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	// Session-only row (no history turns) must still appear on the hub.
	if err := srv.sessions.Set("thread-only-sess", sessionstore.Entry{
		SessionID: "sess-only",
		Project:   "proj",
		LastUser:  "bob#1",
		Goal:      "session without turns",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-sessions"`,
		"Grok Work",
		`href="/sessions"`,
		">Sessions<",
		"/sessions/thread-99",
		"thread-99",
		"proj",
		"alice#0",
		"do a huge refactor",
		"/sessions/thread-only-sess",
		"thread-only-sess",
		"bob#1",
		"no turns recorded yet",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sessions hub missing %q in body (len=%d)", want, len(body))
		}
	}
	// Primary nav: Sessions active; History is not a top-nav work-unit tab.
	if !strings.Contains(body, `>Sessions<`) {
		t.Fatal("nav missing Sessions label")
	}
	if !strings.Contains(body, `class="active"`) {
		t.Fatal("expected Sessions nav active class on hub")
	}
	if strings.Contains(body, `href="/history">History</a>`) || strings.Contains(body, `href="/history" class="active">History`) {
		t.Fatal("History must not be primary top-nav work-unit tab")
	}
	if strings.Contains(body, "Grok Discord") {
		t.Fatal("legacy brand in sessions hub")
	}

	// Detail still works and highlights Sessions.
	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := w.Body.String()
	for _, want := range []string{
		`id="page-session"`,
		"thread-99",
		"Grok Work",
		`href="/sessions"`,
		"please fix the flaky test",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("session detail missing %q", want)
		}
	}
	if strings.Contains(detail, "Grok Discord") {
		t.Fatal("legacy brand on session detail")
	}
}

func TestNavBrandChrome(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	// Primary nav order markers (labels present as nav links).
	for _, want := range []string{
		">Dashboard<",
		">Ship<",
		">Issues<",
		">Sessions<",
		">Worktrees<",
		">Config<",
		"Grok Work",
		"· Grok Work",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("chrome missing %q", want)
		}
	}
	if strings.Contains(body, "Grok Discord") {
		t.Fatal("legacy Grok Discord brand")
	}
	// History is not a primary nav link.
	if strings.Contains(body, `>History</a>`) {
		t.Fatal("History must not appear as primary nav label")
	}
}

func TestShipBoardRendersPRs(t *testing.T) {
	srv, _, _ := testServer(t)
	// Seed a tracked PR on the existing session.
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "sess-99",
		Project:   "proj",
		LastUser:  "alice#0",
		OwnerName: "alice",
		Goal:      "ship a PR",
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/acme/proj/pull/7",
			Number: 7,
			State:  "OPEN",
			Title:  "add feature x",
			Checks: "✓ 1 · ✗ 1",
			Review: "CHANGES_REQUESTED",
			Owner:  "acme",
			Repo:   "proj",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/ship", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-ship"`,
		"acme/proj#7",
		"add feature x",
		"CHANGES_REQUESTED",
		"✗ 1",
		"alice",
		"thread-99",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ship page missing %q in %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/ship?project=proj&state=failing", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("filter status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "acme/proj#7") {
		t.Fatal("filtered ship page missing PR")
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
			path: "/config/projects/users",
			form: url.Values{"name": {"proj"}, "id": {"user-added"}},
			check: func(t *testing.T) {
				if !cfg.AccessAllowed("proj", "user-added", nil) {
					t.Fatal("runtime user missing")
				}
			},
		},
		{
			path: "/config/projects/roles",
			form: url.Values{"name": {"proj"}, "id": {"role-added"}},
			check: func(t *testing.T) {
				if !cfg.AccessAllowed("proj", "x", []string{"role-added"}) {
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
	for _, want := range []string{
		"added", newProj, "ch-added", "Remove", "Add channel map",
		"Grok run limits", "maxTurns", "timeoutMs",
		"Worktree idle cleanup", "worktreeIdleTTLDays", "CI triage", "autoFixCI", "Completion risk paths",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("config page missing %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/config/projects/proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	pbody := w.Body.String()
	for _, want := range []string{"user-added", "role-added", "Members"} {
		if !strings.Contains(pbody, want) {
			t.Fatalf("project config missing %q", want)
		}
	}

	// Settings: Grok run limits
	reqRun := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(url.Values{
		"section":   {"run"},
		"maxTurns":  {"55"},
		"timeoutMs": {"1200000"},
	}.Encode()))
	reqRun.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRun := httptest.NewRecorder()
	h.ServeHTTP(wRun, reqRun)
	if wRun.Code != http.StatusSeeOther && wRun.Code != http.StatusFound {
		t.Fatalf("run settings status=%d body=%s", wRun.Code, wRun.Body.String())
	}
	if cfg.MaxTurnsValue() != 55 || cfg.TimeoutMsValue() != 1_200_000 {
		t.Fatalf("run limits turns=%d timeout=%d", cfg.MaxTurnsValue(), cfg.TimeoutMsValue())
	}

	// Settings: idle TTL
	reqTTL := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(url.Values{
		"section":             {"worktree"},
		"worktreeIdleTTLDays": {"14"},
	}.Encode()))
	reqTTL.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wTTL := httptest.NewRecorder()
	h.ServeHTTP(wTTL, reqTTL)
	if wTTL.Code != http.StatusSeeOther && wTTL.Code != http.StatusFound {
		t.Fatalf("settings status=%d body=%s", wTTL.Code, wTTL.Body.String())
	}
	if cfg.WorktreeIdleTTLDaysValue() != 14 {
		t.Fatalf("ttl days=%d", cfg.WorktreeIdleTTLDaysValue())
	}

	// Settings: CI triage
	reqCI := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(url.Values{
		"section":      {"ci"},
		"autoFixCI":    {"1"},
		"autoFixCIMax": {"3"},
	}.Encode()))
	reqCI.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wCI := httptest.NewRecorder()
	h.ServeHTTP(wCI, reqCI)
	if wCI.Code != http.StatusSeeOther && wCI.Code != http.StatusFound {
		t.Fatalf("ci settings status=%d body=%s", wCI.Code, wCI.Body.String())
	}
	if !cfg.AutoFixCIEnabled() || cfg.AutoFixCIMaxAttempts() != 3 {
		t.Fatalf("autoFix=%v max=%d", cfg.AutoFixCIEnabled(), cfg.AutoFixCIMaxAttempts())
	}

	// Settings: risky globs custom
	reqRisk := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(url.Values{
		"section":              {"risky"},
		"riskyPathGlobs":       {"**/auth/**\n**/deploy/**"},
		"riskyPathUseDefault":  {""},
	}.Encode()))
	reqRisk.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRisk := httptest.NewRecorder()
	h.ServeHTTP(wRisk, reqRisk)
	if wRisk.Code != http.StatusSeeOther && wRisk.Code != http.StatusFound {
		t.Fatalf("risky settings status=%d", wRisk.Code)
	}
	if !cfg.RiskyPathGlobsConfigured() || len(cfg.RiskyPathGlobsEffective()) != 2 {
		t.Fatalf("risky globs=%v", cfg.RiskyPathGlobsEffective())
	}

	// Settings: team board
	reqBoard := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(url.Values{
		"section":            {"board"},
		"boardStaleDays":     {"7"},
		"boardDigestChannel": {"999888777"},
	}.Encode()))
	reqBoard.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wBoard := httptest.NewRecorder()
	h.ServeHTTP(wBoard, reqBoard)
	if wBoard.Code != http.StatusSeeOther && wBoard.Code != http.StatusFound {
		t.Fatalf("board settings status=%d body=%s", wBoard.Code, wBoard.Body.String())
	}
	if cfg.BoardStaleDaysValue() != 7 || cfg.BoardDigestChannelValue() != "999888777" {
		t.Fatalf("board stale=%d channel=%q", cfg.BoardStaleDaysValue(), cfg.BoardDigestChannelValue())
	}

	// Removes
	removes := []postCase{
		{
			path: "/config/projects/users/remove",
			form: url.Values{"name": {"proj"}, "id": {"user-added"}},
			check: func(t *testing.T) {
				if cfg.AccessAllowed("proj", "user-added", nil) {
					t.Fatal("user still allowed")
				}
			},
		},
		{
			path: "/config/projects/roles/remove",
			form: url.Values{"name": {"proj"}, "id": {"role-added"}},
			check: func(t *testing.T) {
				if cfg.AccessAllowed("proj", "x", []string{"role-added"}) {
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
		Projects       config.ProjectsMap `json:"projects"`
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
	// project members removed — check via AccessAllowed already above
	_ = disk
}

// HTMXAwareRedirect: non-boosted htmx POSTs get HX-Redirect + 204 (htmx does not
// follow 3xx into a fragment). Boosted posts still get a normal 3xx.
func TestHTMXAwareRedirect(t *testing.T) {
	srv, _, dir := testServer(t)
	h := srv.Handler()
	newProj := filepath.Join(dir, "htmx-proj")
	if err := os.MkdirAll(newProj, 0o755); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"htmx-proj"}, "path": {newProj}}.Encode()

	t.Run("partial htmx POST", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/config/projects", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		// addProject lands on the new project's own settings page.
		loc := w.Header().Get("HX-Redirect")
		if !strings.HasPrefix(loc, "/config/projects/htmx-proj?") || !strings.Contains(loc, "ok=") {
			t.Fatalf("HX-Redirect=%q", loc)
		}
		if w.Header().Get("Location") != "" {
			t.Fatalf("unexpected Location=%q", w.Header().Get("Location"))
		}
	})

	t.Run("boosted htmx POST", func(t *testing.T) {
		// Second project so the name is unique.
		form2 := url.Values{"name": {"htmx-boost"}, "path": {newProj}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/config/projects", strings.NewReader(form2))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("HX-Boosted", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if w.Header().Get("HX-Redirect") != "" {
			t.Fatalf("boosted path should not set HX-Redirect, got %q", w.Header().Get("HX-Redirect"))
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "/config/projects/htmx-boost?") {
			t.Fatalf("Location=%q", loc)
		}
	})
}

// Project-scoped saves round-trip back to the project settings page; unknown
// projects bounce to the config hub with an error.
func TestProjectConfigPage(t *testing.T) {
	srv, cfg, _ := testServer(t)
	h := srv.Handler()

	// Unknown project → config hub with err.
	req := httptest.NewRequest(http.MethodGet, "/config/projects/nope", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("unknown project status=%d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config?") || !strings.Contains(loc, "err=") {
		t.Fatalf("unknown project Location=%q", loc)
	}

	// Save repos → back to the project page with flash.
	form := url.Values{"name": {"proj"}, "repos": {"acme/app\nacme/api"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/github", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set repos status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("set repos Location=%q", loc)
	}

	// Channel map from the project page round-trips with return_to=project.
	form = url.Values{"channelId": {"ch-proj-2"}, "project": {"proj"}, "return_to": {"project"}}
	req = httptest.NewRequest(http.MethodPost, "/config/channels", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj?") {
		t.Fatalf("add channel Location=%q", loc)
	}
	if p, ok := cfg.ChannelProject("ch-proj-2"); !ok || p != "proj" {
		t.Fatalf("channel not mapped: %q %v", p, ok)
	}

	// Project page renders the saved state.
	req = httptest.NewRequest(http.MethodGet, "/config/projects/proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("project page status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-project-config"`,
		"acme/app",
		"ch-proj-2",
		`name="return_to"`,
		"Remove project",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("project page missing %q", want)
		}
	}
}

func TestWorktreePruneRoutes(t *testing.T) {
	srv, cfg, dir := testServer(t)
	h := srv.Handler()

	// Init a real git repo as the project path so prune can run Remove.
	repo := filepath.Join(dir, "proj")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README")
	runGit("commit", "-m", "init")

	threadID := "wt-web-1"
	tr, err := gitworktree.Ensure(context.Background(), repo, cfg.DataDir, "proj", threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set(threadID, sessionstore.Entry{
		SessionID:      "sw",
		Project:        "proj",
		Cwd:            tr.Path,
		MainCwd:        repo,
		WorktreeBranch: tr.Branch,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/worktrees", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), threadID) {
		t.Fatalf("worktrees page missing thread: %s", w.Body.String())
	}

	form := url.Values{"threadId": {threadID}}
	req = httptest.NewRequest(http.MethodPost, "/worktrees/prune", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("prune status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}

	// Idle prune endpoint (no idle trees is fine).
	days := 30
	cfg.WorktreeIdleTTLDays = &days
	req = httptest.NewRequest(http.MethodPost, "/worktrees/prune-idle", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("prune-idle status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("location=%q", loc)
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
