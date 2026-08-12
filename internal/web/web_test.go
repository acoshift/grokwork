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
	"sync"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
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
		Channels:   map[string]string{"ch": "proj"},
		GrokBin:    "grok",
		MaxTurns:   40,
		TimeoutMs:  1000,
		HTTPListen: "127.0.0.1:0",
		ConfigPath: cfgPath,
		DataDir:    filepath.Join(dir, "data"),
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
		Goal:      "please fix the flaky test",
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

func TestSessionVerifyPanelAndShipFromCase(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "sess-99", Project: "proj",
		LastVerify: &sessionstore.LastVerify{
			Name: "unit", OK: false, ExitCode: 1, At: "2026-07-23T00:00:00Z",
			Summary: "unit fail · 10ms", LogTail: "FAIL: TestX",
		},
		Mode: "case", Phase: sessionstore.PhaseShipping, CustomerTitle: "Pay wall",
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/7", Number: 7, State: "OPEN",
			Title: "fix pay", Owner: "acme", Repo: "app",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="session-verify-panel"`, "unit", "FAIL: TestX", "status-error",
		// Tracked PR → in-app detail (header + work-unit list).
		`href="/prs/acme/app/7?project=proj">PR #7</a>`,
		`href="/prs/acme/app/7?project=proj">acme/app#7</a>`,
		"fix pay",
		"Pull requests",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session missing %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/proj/ship", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ship status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "from case") {
		t.Fatal("ship board missing from case badge")
	}
}

func TestPagesRender(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	cases := []struct {
		path   string
		marker string
	}{
		{"/", `id="page-home"`},
		{"/", `class="proj-card"`},
		{"/", `href="/projects/proj"`},
		// Font loading: preload latin faces + block (not swap) so refresh
		// does not flash system font before Inter arrives.
		{"/", `rel="preload" href="/static/fonts/inter-latin-var.woff2"`},
		{"/", `font-display: block`},
		{"/ship", `id="page-ship"`},
		{"/sessions", `id="page-sessions"`},
		{"/history", `id="page-history"`},
		{"/worktrees", `id="page-worktrees"`},
		{"/worktrees", "Prune idle now"},
		// Search results render as a full lead view even with no query.
		{"/search", `id="page-search"`},
		{"/search", `id="search-form"`},
		// Self-service account page. With auth off there is no account, and the
		// page says so instead of rendering buttons that would refuse.
		{"/account", `id="page-account"`},
		{"/account", `id="account-anonymous"`},
		// Cross-project deploy board: renders with no runs on disk at all.
		{"/deploys", `id="page-deploys-board"`},
		{"/deploys", `id="live-deploys-board"`},
		// Project workspace pages (project-first UX).
		{"/projects/proj", `id="page-project-overview"`},
		{"/projects/proj", `id="live-project-pulse"`},
		{"/projects/proj/start", `id="page-start"`},
		{"/projects/proj/ship", `id="page-ship"`},
		{"/projects/proj/cases", `id="page-cases"`},
		{"/projects/proj/cases", `id="case-pipeline"`},
		{"/projects/proj/sessions", `id="page-sessions"`},
		{"/projects/proj/worktrees", `id="page-worktrees"`},
		// Deploys renders its own chrome even when the project path is not a git
		// checkout (this fixture is not): the page reports that inline.
		{"/projects/proj/deploys", `id="page-deploys"`},
		// Files same contract: page chrome with no bucket configured.
		{"/projects/proj/files", `id="page-project-files"`},
		// Actions same contract: page chrome even when gh/catalog is unavailable.
		{"/projects/proj/actions", `id="page-actions"`},
		// Config hub: grouped drill-in rows; sections live on focused pages.
		{"/cases", `id="page-cases"`},
		{"/cases", `id="case-pipeline"`},
		{"/config", `id="page-config"`},
		{"/config", `href="/config/bot"`},
		{"/config", `href="/config/channels"`},
		{"/config", `href="/config/run"`},
		{"/config", `href="/config/agent"`},
		{"/config", `href="/config/skills"`},
		{"/config", `href="/config/risky"`},
		{"/config", `href="/config/board"`},
		{"/config", `href="/config/ci"`},
		{"/config", `href="/config/pr-links"`},
		{"/config", `href="/config/worktrees"`},
		{"/config", `href="/config/storage"`},
		{"/config", `href="/config/projects/new"`},
		{"/config", `action="/config/resume"`},
		{"/config", `href="/config/projects/proj"`},
		{"/config/skills", `id="page-config-skills"`},
		{"/config/skills", "Coding-agent skill packages"},
		{"/config/skills", `class="table-scroll m-hide"`},
		{"/config/skills", `class="m-rows"`},
		{"/config/bot", `id="page-config-bot"`},
		{"/config/bot", `id="bot-invite"`},
		{"/config/bot", "discord.com/oauth2/authorize"},
		{"/config/bot", "Pin Messages"},
		{"/config/bot", "Open / re-authorize"},
		{"/config/bot", "424242424242424242"},
		{"/config/bot", "Default Discord guild"},
		{"/config/channels", `id="page-config-channels"`},
		{"/config/channels", `id="live-config-channels"`},
		{"/config/run", `id="page-config-run"`},
		{"/config/run", `name="maxTurns"`},
		{"/config/run", `name="timeoutMs"`},
		{"/config/run", `placeholder="30m"`},
		{"/config/run", `name="maxConcurrentRuns"`},
		{"/config/run", `name="maxConcurrentRunsUser"`},
		{"/config/run", `placeholder="unlimited"`},
		{"/config/agent", `id="page-config-agent"`},
		{"/config/agent", `name="agent"`},
		{"/config/agent", `name="claudeBin"`},
		{"/config/agent", `name="grokBin"`},
		{"/config/agent", `name="model"`},
		{"/config/agent", `name="summarizeModel"`},
		{"/config/agent", `name="claudeIncludeAnthropicEnv"`},
		// Models are picked from a grouped dropdown, not typed free-form; the
		// optgroup label is how the inferred agent shows at the point of choice.
		{"/config/agent", `<optgroup label="Grok">`},
		{"/config/agent", `<optgroup label="Claude">`},
		{"/config/agent", `<option value="grok-4.5"`},
		{"/config/agent", `<option value="claude-opus-5"`},
		{"/config/agent", `<option value="claude-sonnet-5"`},
		{"/config/worktrees", `id="page-config-worktrees"`},
		{"/config/worktrees", `name="worktreeIdleTTLDays"`},
		{"/config/worktrees", `name="terminalSessionTTLDays"`},
		{"/config/board", `id="page-config-board"`},
		{"/config/notify", `id="page-config-notify"`},
		{"/config", `href="/config/notify"`},
		{"/config", "Run notifications"},
		{"/config/ci", `id="page-config-ci"`},
		{"/config/pr-links", `id="page-config-prlinks"`},
		{"/config/risky", `id="page-config-risky"`},
		{"/config/storage", `id="page-config-storage"`},
		{"/config/storage", `name="gcsBucket"`},
		{"/config/projects/new", `id="page-config-project-new"`},
		// Per-project settings: four sub-tab pages (Access is the default).
		{"/config/projects/proj", `id="page-project-config"`},
		{"/config/projects/proj", `id="project-config-tabs"`},
		{"/config/projects/proj", "Team policy"},
		{"/config/projects/proj", "name=\"safeTeamMode\""},
		{"/config/projects/proj", `id="project-members"`},
		{"/config/projects/proj", `href="/config/projects/proj/workflow"`},
		{"/config/projects/proj", `href="/config/projects/proj/integrations"`},
		{"/config/projects/proj", `href="/config/projects/proj/danger"`},
		{"/config/projects/proj/workflow", `id="page-project-config-workflow"`},
		{"/config/projects/proj/workflow", "Shipping"},
		{"/config/projects/proj/workflow", "name=\"directToPrimary\""},
		{"/config/projects/proj/workflow", "name=\"defaultMode\""},
		{"/config/projects/proj/workflow", "Verify commands"},
		{"/config/projects/proj/workflow", "name=\"verifyCommands\""},
		{"/config/projects/proj/workflow", "Suggest with Grok"},
		{"/config/projects/proj/workflow", "data-grok-stream"},
		{"/config/projects/proj/workflow", "/config/projects/verify/generate"},
		{"/config", `id="gw-stream-modal"`},  // reusable stream modal shell
		{"/config", `id="gw-confirm-modal"`}, // confirm/alert modal shell
		{"/config/projects/proj/integrations", `id="page-project-config-integrations"`},
		{"/config/projects/proj/integrations", "Discord guild ID"},
		{"/config/projects/proj/integrations", "name=\"guildId\""},
		{"/config/projects/proj/integrations", "GitHub repositories"},
		{"/config/projects/proj/integrations", "LINEAR_API_KEY_PROJ"},
		{"/config/projects/proj/danger", `id="page-project-config-danger"`},
		{"/config/projects/proj/danger", "Remove project"},
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
				// Scope-aware sidebar swaps with every boosted response.
				`hx-select-oob="#side-nav"`,
				`id="side-nav"`,
				`data-scope=`,
				`hx-swap="outerHTML show:none focus-scroll:false"`,
				`hx-inherit="*"`,
				// disableInheritance must be set before processNode (meta) and
				// again on script onload (belt-and-suspenders). Without it,
				// live-region hx-target/hx-select nest a full page on boost.
				`name="htmx-config"`,
				`disableInheritance`,
				`scrollIntoViewOnBoost`,
				`onload=`,
				// Runtime guards if inheritance still leaks onto child links.
				`htmx:beforeSwap`,
				`selectOverride`,
				`querySelectorAll("#side-nav")`,
				`boostScrollByPath`,
				// Drill-in (list→issue detail) always lands at y=0; leave-path
				// is needed because navFrom advances before afterSettle.
				`boostScrollFromPath`,
				`applyBoostScrollY`,
				// SSE table reloads: keep .table-scroll horizontal position.
				`_tableScrollX`,
				// SSE live regions: keep window Y when content grows.
				// pageMax > 0 is load-bearing: short shells (issues hx-load)
				// must not pin-to-bottom when the tall table arrives.
				`_pageScrollY`,
				`pageMax > 0`,
				// Mid-session SSE reconnect catch-up (rev compare → partial refresh).
				`lastLiveRevs`,
				`applyLiveRevs`,
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
			{"/partials/home/projects", `id="proj-grid"`, "dashboard"},
			{"/partials/home/runs", `id="runs-wrap"`, "dashboard"},
			{"/partials/projects/pulse?project=proj", `id="pulse"`, "dashboard"},
			{"/partials/ship/stats", "CI failing", "ship"},
			{"/partials/ship/table", "Pull requests", "ship"},
			{"/partials/cases/pipeline?project=proj", `id="case-pipeline"`, "cases"},
			{"/partials/cases/list?project=proj", `id="cases-list"`, "cases"},
			{"/partials/history/table", "thread-99", "history"},
			{"/partials/history/turns/thread-99", `id="turns"`, "history"},
			{"/partials/sessions/thread-99", `id="turns"`, "dashboard"},
			{"/partials/worktrees/table", "All worktrees", "worktrees"},
			{"/partials/issues/table?project=proj&owner=acme&repo=app", "No issues.", ""},
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
				"<nav",
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
			{"/", []string{`hx-trigger="sse:dashboard, sse:ship, sse:history"`, `hx-trigger="sse:dashboard"`}},
			{"/projects/proj", []string{`hx-trigger="sse:dashboard, sse:ship, sse:cases"`}},
			{"/projects/proj/cases", []string{`hx-trigger="sse:cases"`, "/partials/cases/pipeline?project=proj", "/partials/cases/list?project=proj"}},
			{"/projects/proj/worktrees", []string{`hx-trigger="sse:worktrees"`, "/partials/worktrees/table?project=proj"}},
			{"/ship", []string{`hx-trigger="sse:ship"`}},
			{"/history", []string{`hx-trigger="sse:history"`}},
			{"/history/thread-99", []string{`hx-trigger="sse:history"`}},
			{"/sessions/thread-99", []string{`hx-trigger="sse:dashboard, sse:history"`, `/partials/sessions/thread-99`}},
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
		for _, path := range []string{
			"/static/htmx.min.js",
			"/static/sse.js",
			"/static/fonts/inter-latin-var.woff2",
			"/static/fonts/ibm-plex-mono-400.woff2",
		} {
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

		// Fonts: year cache so refresh reuses the face and skips the system-font flash.
		req := httptest.NewRequest(http.MethodGet, "/static/fonts/inter-latin-var.woff2", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=31536000") || !strings.Contains(cc, "immutable") {
			t.Fatalf("font Cache-Control=%q want long immutable", cc)
		}
		// Non-font static still cached, but shorter (no content hash in URL).
		req = httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
			t.Fatalf("js Cache-Control=%q want max-age=86400", cc)
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
		`class="bubble-body md"`,
		"<p>please fix the flaky test</p>",
		"<p>I fixed it by waiting for the race.</p>",
		"ship a PR",
		"Opened",
		`href="https://example.com/pr/1"`,
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

// TestTranscriptNamesSessionAgent pins that reply bubbles and the run-status
// line name the CLI the session is stamped on. A thread stamped on claude
// captioned "Grok" misattributes every answer in it, and the stamp is
// immutable, so the whole transcript is wrong — not just the newest turn.
func TestTranscriptNamesSessionAgent(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	paths := []string{"/sessions/thread-99", "/history/thread-99"}

	// Unstamped: predates the feature, so it stays grok (threadCLI's rule).
	for _, p := range paths {
		body := getBody(t, h, p)
		if !strings.Contains(body, `class="bubble-label">Grok`) {
			t.Fatalf("%s: unstamped session should label bubbles Grok: %s", p, body)
		}
	}

	if _, ok, err := srv.sessions.Patch("thread-99", func(e *sessionstore.Entry) {
		e.Agent, e.Model = "claude", "sonnet"
	}); err != nil || !ok {
		t.Fatalf("stamp claude: ok=%v err=%v", ok, err)
	}
	for _, p := range paths {
		body := getBody(t, h, p)
		if !strings.Contains(body, `class="bubble-label">Claude`) {
			t.Fatalf("%s: claude session missing Claude bubble label: %s", p, body)
		}
		if strings.Contains(body, `class="bubble-label">Grok`) {
			t.Fatalf("%s: claude session still labels a bubble Grok: %s", p, body)
		}
	}
	// Same resolution drives the idle run-status line on the session page.
	if body := getBody(t, h, "/sessions/thread-99"); !strings.Contains(body, "No active Claude run.") {
		t.Fatalf("session page run-status line not agent-aware: %s", body)
	}
}

func getBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
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
	// Closed eng session with terminal PR must keep PR link + closed state on the list.
	// Recent turn stamp so it still matches the default Active filter.
	if err := srv.sessions.Set("thread-closed", sessionstore.Entry{
		SessionID: "sess-closed",
		Project:   "proj",
		Label:     sessionstore.LabelDone,
		LastUser:  "carol#2",
		Goal:      "merged ship",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		PRs: []sessionstore.TrackedPR{{
			URL: "https://github.com/acme/app/pull/42", Number: 42, State: "MERGED",
			Title: "ship feature", Owner: "acme", Repo: "app",
		}},
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
		"please fix the flaky test", // sticky goal, not last turn prompt
		">Goal</th>",
		"/sessions/thread-only-sess",
		"thread-only-sess",
		"bob#1",
		"session without turns",
		// Closed session: state + PR columns.
		"/sessions/thread-closed",
		`status-done">done</span>`,
		`href="/prs/acme/app/42?project=proj">#42 · MERGED</a>`,
		">State</th>",
		">PR</th>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sessions hub missing %q in body (len=%d)", want, len(body))
		}
	}
	if strings.Contains(body, ">Last prompt</th>") {
		t.Fatal("sessions hub must use Goal column, not Last prompt")
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

	// Detail still works and highlights Sessions. ← Sessions uses the
	// session's project even when the URL has no ?project= (global shell).
	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-99", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := w.Body.String()
	for _, want := range []string{
		`id="page-session"`,
		`id="live-session"`,
		`hx-trigger="sse:dashboard, sse:history"`,
		`/partials/sessions/thread-99`,
		"thread-99",
		"Grok Work",
		`href="/projects/proj/sessions">← Sessions</a>`,
		`class="bubble-body md"`,
		"<p>please fix the flaky test</p>",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("session detail missing %q", want)
		}
	}
	if strings.Contains(detail, "Grok Discord") {
		t.Fatal("legacy brand on session detail")
	}

	// Closed session detail preserves PR link + done state.
	req = httptest.NewRequest(http.MethodGet, "/sessions/thread-closed?project=proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("closed session status=%d", w.Code)
	}
	closed := w.Body.String()
	for _, want := range []string{
		`id="page-session"`,
		`status-done">done</span>`,
		`href="/prs/acme/app/42?project=proj">PR #42</a>`,
		`href="/prs/acme/app/42?project=proj">acme/app#42</a>`,
		`status-merged">MERGED</span>`,
		"ship feature",
	} {
		if !strings.Contains(closed, want) {
			t.Fatalf("closed session detail missing %q", want)
		}
	}
}

func TestSessionsFilter(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	// thread-99 (from testServer) has no explicit label → effective open.
	// Terminal rows need a recent turn stamp to stay on the Active view
	// (Set no longer invents UpdatedAt).
	recent := time.Now().UTC().Format(time.RFC3339)
	if err := srv.sessions.Set("thread-done", sessionstore.Entry{
		SessionID: "sess-done",
		Project:   "proj",
		Label:     sessionstore.LabelDone,
		LastUser:  "carol#2",
		Goal:      "merged ship work",
		UpdatedAt: recent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("thread-case-closed", sessionstore.Entry{
		SessionID:     "sess-case",
		Project:       "proj",
		Mode:          "case",
		Phase:         sessionstore.PhaseClosed,
		Resolution:    "answered",
		CustomerTitle: "login broken for beta users",
		UpdatedAt:     recent,
	}); err != nil {
		t.Fatal(err)
	}

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	has := func(body, path, frag string) {
		t.Helper()
		if !strings.Contains(body, frag) {
			t.Fatalf("GET %s missing %q", path, frag)
		}
	}
	hasNot := func(body, path, frag string) {
		t.Helper()
		if strings.Contains(body, frag) {
			t.Fatalf("GET %s must not contain %q", path, frag)
		}
	}

	// Default view is Active: in-flight rows plus freshly finished ones
	// (terminal fixtures carry a recent turn stamp above).
	body := get("/sessions")
	has(body, "/sessions", `id="sessions-filters"`)
	has(body, "/sessions", `name="project"`)
	has(body, "/sessions", `name="state"`)
	has(body, "/sessions", `name="q"`)
	has(body, "/sessions", `<option value="proj"`)
	has(body, "/sessions", "/sessions/thread-99")
	has(body, "/sessions", "/sessions/thread-done")
	has(body, "/sessions", "/sessions/thread-case-closed")

	// State filter matches the effective lifecycle label.
	body = get("/sessions?state=done")
	has(body, "/sessions?state=done", "/sessions/thread-done")
	hasNot(body, "/sessions?state=done", "/sessions/thread-99")
	hasNot(body, "/sessions?state=done", "/sessions/thread-case-closed")

	// Explicit Active matches the default view: terminal rows updated within
	// the last 24h stay visible (recency itself is unit-tested with a fixed
	// clock in TestSessionsActiveRecency).
	body = get("/sessions?state=active")
	has(body, "/sessions?state=active", "/sessions/thread-99")
	has(body, "/sessions?state=active", "/sessions/thread-done")
	has(body, "/sessions?state=active", "/sessions/thread-case-closed")

	// All states shows everything regardless of age.
	body = get("/sessions?state=all")
	has(body, "/sessions?state=all", "/sessions/thread-99")
	has(body, "/sessions?state=all", "/sessions/thread-done")
	has(body, "/sessions?state=all", "/sessions/thread-case-closed")

	// Closed cases match only the dedicated closed option.
	body = get("/sessions?state=closed")
	has(body, "/sessions?state=closed", "/sessions/thread-case-closed")
	hasNot(body, "/sessions?state=closed", "/sessions/thread-99")
	hasNot(body, "/sessions?state=closed", "/sessions/thread-done")

	// Free-text search over goal and last prompt.
	body = get("/sessions?q=flaky")
	has(body, "/sessions?q=flaky", "/sessions/thread-99")
	hasNot(body, "/sessions?q=flaky", "/sessions/thread-done")
	body = get("/sessions?q=refactor") // still matches last turn prompt
	has(body, "/sessions?q=refactor", "/sessions/thread-99")
	body = get("/sessions?q=beta")
	has(body, "/sessions?q=beta", "/sessions/thread-case-closed")
	hasNot(body, "/sessions?q=beta", "/sessions/thread-99")

	// Unknown state values fall back to the default Active view (no 4xx).
	body = get("/sessions?state=bogus")
	has(body, "/sessions?state=bogus", "/sessions/thread-99")
	has(body, "/sessions?state=bogus", "/sessions/thread-done")

	// No-match search shows the filtered empty state, not the onboarding copy.
	body = get("/sessions?q=zzz-no-match")
	has(body, "/sessions?q=zzz-no-match", "No sessions match this filter")
	hasNot(body, "/sessions?q=zzz-no-match", "Work units appear after")

	// Workspace page: same filters, project fixed via path (no project select).
	body = get("/projects/proj/sessions?state=done")
	has(body, "/projects/proj/sessions?state=done", `id="sessions-filters"`)
	hasNot(body, "/projects/proj/sessions?state=done", `name="project"`)
	has(body, "/projects/proj/sessions?state=done", "/sessions/thread-done")
	hasNot(body, "/projects/proj/sessions?state=done", "/sessions/thread-99")
}

func TestSessionsActiveRecency(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	rows := []history.Summary{
		{ThreadID: "open-old", Label: "open", UpdatedAt: "2026-06-01T00:00:00Z"},
		{ThreadID: "done-fresh", Label: "done", UpdatedAt: "2026-07-24T02:00:00Z"},
		{ThreadID: "done-stale", Label: "done", UpdatedAt: "2026-07-23T02:00:00Z"},
		{ThreadID: "abandoned-fresh", Label: "abandoned", UpdatedAt: "2026-07-24T11:00:00Z"},
		{ThreadID: "abandoned-stale", Label: "abandoned", UpdatedAt: "2026-07-20T00:00:00Z"},
		{ThreadID: "case-fresh", Mode: "case", Phase: "closed", Label: "blocked", UpdatedAt: "2026-07-24T00:00:00Z"},
		{ThreadID: "case-stale", Mode: "case", Phase: "closed", Label: "blocked", UpdatedAt: "2026-07-22T00:00:00Z"},
		{ThreadID: "done-notime", Label: "done"},
		// Shipped work: all PRs terminal but label still needs_review (no auto-done).
		{ThreadID: "merged-fresh", Label: "needs_review", AllPRsTerminal: true, UpdatedAt: "2026-07-24T10:00:00Z"},
		{ThreadID: "merged-stale", Label: "needs_review", AllPRsTerminal: true, UpdatedAt: "2026-07-20T00:00:00Z"},
		// Live run on a fully-merged unit still counts as active.
		{ThreadID: "merged-running", Label: "needs_review", AllPRsTerminal: true, Running: true, UpdatedAt: "2026-07-01T00:00:00Z"},
		// Open case with terminal PRs is still in flight (phase lifecycle owns it).
		{ThreadID: "case-pr-old", Mode: "case", Phase: "fixing", Label: "needs_review", AllPRsTerminal: true, UpdatedAt: "2026-06-01T00:00:00Z"},
		// History-only (no session overlay → empty Label): not in-flight.
		{ThreadID: "hist-fresh", UpdatedAt: "2026-07-24T11:30:00Z"},
		{ThreadID: "hist-stale", UpdatedAt: "2026-06-01T00:00:00Z"},
	}
	ids := func(rows []history.Summary) string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ThreadID)
		}
		return strings.Join(out, " ")
	}

	// Active: true in-flight regardless of age; settled only within 24h;
	// live Running always; settled-without-timestamp never.
	got := ids(filterSessionRows(rows, sessionFilters{State: "active"}, now))
	if want := "open-old done-fresh abandoned-fresh case-fresh merged-fresh merged-running case-pr-old hist-fresh"; got != want {
		t.Fatalf("active: got %q want %q", got, want)
	}
	// All: everything, any age.
	got = ids(filterSessionRows(rows, sessionFilters{State: "all"}, now))
	if got != ids(rows) {
		t.Fatalf("all: got %q want every row", got)
	}
	// Explicit label filters are age-independent…
	got = ids(filterSessionRows(rows, sessionFilters{State: "done"}, now))
	if want := "done-fresh done-stale done-notime"; got != want {
		t.Fatalf("done: got %q want %q", got, want)
	}
	// …and closed cases match only "closed" (frozen label never leaks).
	got = ids(filterSessionRows(rows, sessionFilters{State: "closed"}, now))
	if want := "case-fresh case-stale"; got != want {
		t.Fatalf("closed: got %q want %q", got, want)
	}
	got = ids(filterSessionRows(rows, sessionFilters{State: "blocked"}, now))
	if got != "" {
		t.Fatalf("blocked: got %q want no rows", got)
	}
	// needs_review still lists shipped units (label filter ≠ Active).
	got = ids(filterSessionRows(rows, sessionFilters{State: "needs_review"}, now))
	if want := "merged-fresh merged-stale merged-running case-pr-old"; got != want {
		t.Fatalf("needs_review: got %q want %q", got, want)
	}
	// History-only defaults to open for the open label filter.
	got = ids(filterSessionRows(rows, sessionFilters{State: "open"}, now))
	if want := "open-old hist-fresh hist-stale"; got != want {
		t.Fatalf("open: got %q want %q", got, want)
	}
}

func TestSessionsListShowsRunningForActiveJob(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	// Lifecycle label alone is "in progress" after a turn; the list must also
	// show a distinct live "running" badge while an agent job is active —
	// Claude PR reviews (and long Grok turns) look idle without it.
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		Project: "proj", Label: sessionstore.LabelInProgress, Goal: "review PR",
	}); err != nil {
		t.Fatal(err)
	}
	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj", "prompt", "live…"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `badge live">running`) {
		t.Fatalf("sessions list missing live running badge:\n%s", body)
	}
	if !strings.Contains(body, `value="running"`) {
		t.Fatal("sessions list missing Running state filter option")
	}

	// Filter state=running keeps only busy rows.
	req = httptest.NewRequest(http.MethodGet, "/sessions?state=running", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body = w.Body.String()
	if !strings.Contains(body, "/sessions/thread-99") {
		t.Fatalf("state=running dropped the busy session:\n%s", body)
	}
}

func TestSessionDetailStreamsLiveTurn(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	if err := bot.SeedActiveRunForTest(srv.bot, "thread-99", "proj",
		"please stream this turn", "Here is the live reply so far…"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bot.FinishRunForTest(srv.bot, "thread-99") })

	// Full page
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="turn-live"`,
		`badge live">running`,
		`badge live">streaming`,
		"please stream this turn",
		"Here is the live reply so far…",
		`id="live-stream-body"`,
		"editing files",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page missing %q", want)
		}
	}

	// SSE partial
	req = httptest.NewRequest(http.MethodGet, "/partials/sessions/thread-99", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial status=%d", w.Code)
	}
	partial := w.Body.String()
	if !strings.Contains(partial, "Here is the live reply so far…") {
		t.Fatal("partial missing live text")
	}
	if strings.Contains(partial, "<nav") || strings.Contains(partial, "sse-status") {
		t.Fatal("partial leaked layout chrome")
	}
	// Continue form must stay on the full page only (outside live-region).
	if strings.Contains(partial, "session-continue-form") {
		t.Fatal("continue form must not be in live partial")
	}
}

func TestNavBrandChrome(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	// Global shell: launcher + cross-project lead views. Feature-first hubs
	// (Issues/Commits pickers) are gone — projects are picked first.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`data-scope=""`,
		">Projects<",
		">Ship<",
		">Cases<",
		">Sessions<",
		">Worktrees<",
		">Deploys<",
		">Config<",
		"Across projects",
		"Grok Work",
		"· Grok Work",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("global chrome missing %q", want)
		}
	}
	for _, ban := range []string{">Dashboard<", ">Issues<", ">Commits<", ">History</a>", "Grok Discord"} {
		if strings.Contains(body, ban) {
			t.Fatalf("global chrome must not contain %q", ban)
		}
	}

	// Workspace shell: scoped nav + switcher; global lead views hidden.
	req = httptest.NewRequest(http.MethodGet, "/projects/proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body = w.Body.String()
	for _, want := range []string{
		`data-scope="proj"`,
		`class="proj-switch"`,
		"All projects",
		">Overview<",
		">Start task<",
		">Ship<",
		">Cases<",
		">Issues<",
		">Commits<",
		">Files<",
		">Deploys<",
		">Actions<",
		">Sessions<",
		">Worktrees<",
		">Settings<",
		`href="/projects/proj/start"`,
		`href="/projects/proj/ship"`,
		`href="/projects/proj/cases"`,
		`href="/projects/proj/issues"`,
		`href="/projects/proj/commits"`,
		`href="/projects/proj/files"`,
		`href="/projects/proj/deploys"`,
		`href="/projects/proj/actions"`,
		`href="/projects/proj/sessions"`,
		`href="/projects/proj/worktrees"`,
		`href="/config/projects/proj"`,
		// Overview is the active workspace tab (bare-label contract).
		`class="active">Overview</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("workspace chrome missing %q", want)
		}
	}
	// The desktop section list (.nav-links) stays workspace-only; the phone
	// tab bar that follows it is scoped to the same project's sections, and
	// the top-bar back chip is the only route to the global shell.
	navStart := strings.Index(body, `id="nav-links"`)
	tabStart := strings.Index(body, `id="tab-bar"`)
	if navStart == -1 || tabStart == -1 || tabStart < navStart {
		t.Fatalf("workspace chrome missing nav-links/tab-bar (nav=%d tab=%d)", navStart, tabStart)
	}
	if strings.Contains(body[navStart:tabStart], ">Projects<") {
		t.Fatal("workspace section nav must not show the global Projects link")
	}
	tabbar := body[tabStart:]
	if end := strings.Index(tabbar, "</nav>"); end != -1 {
		tabbar = tabbar[:end]
	}
	for _, want := range []string{
		`data-icon="overview" class="active">Overview</a>`,
		`href="/projects/proj/ship" data-icon="ship" class="">Ship</a>`,
		`href="/projects/proj/cases" data-icon="cases" class="">Cases</a>`,
		`href="/projects/proj/sessions" data-icon="sessions" class="">Sessions</a>`,
		`href="/projects/proj/reviews" data-icon="reviews" class="">Reviews</a>`,
		`class="ws-back" href="/"`,
		`class="ws-start" href="/projects/proj/start"`,
	} {
		if !strings.Contains(tabbar, want) {
			t.Fatalf("workspace tab bar missing %q", want)
		}
	}
	// Config is deliberately not a tab in either scope (rarely used; it keeps
	// a drill-in row on the Projects home / workspace Browse instead).
	if strings.Contains(tabbar, ">Projects</a>") || strings.Contains(tabbar, ">Settings</a>") {
		t.Fatal("workspace tab bar must not contain global Projects or Settings tabs")
	}
}

// TestProjectSettingsHasNoConfigCrumb pins the removal of the "← Config" link.
// /config/projects/{p} scopes the shell to the project workspace (see the
// /config/projects/proj row in TestNavScopeRules), so the sidebar is the way
// out and it leads back to the project — the global hub is a sibling scope,
// not a parent. If that scoping ever regresses, this page loses its only
// escape, so the two tests are meant to be read together.
func TestProjectSettingsHasNoConfigCrumb(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	for _, path := range []string{
		"/config/projects/proj",
		"/config/projects/proj/workflow",
		"/config/projects/proj/integrations",
		"/config/projects/proj/danger",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, "← Config") {
			t.Fatalf("%s still renders the Config crumb", path)
		}
		// The workspace sidebar is what replaced it.
		if !strings.Contains(body, `data-scope="proj"`) {
			t.Fatalf("%s lost its workspace shell — nothing links out now", path)
		}
	}
}

// TestNavScopeRules pins the URL→shell-scope contract (mirrored by the layout
// JS navScopeOf): path scopes /projects/… and /config/projects/…;
// ?project= scopes only /sessions/{id…}, /history/{id…}, and /prs/… detail
// pages; global list pages using ?project= as a data filter stay global;
// unknown projects fall back to the global shell.
func TestNavScopeRules(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	cases := []struct {
		path  string
		scope string
	}{
		{"/", ""},
		{"/ship", ""},
		{"/ship?project=proj", ""}, // data filter, not workspace scope
		{"/cases", ""},
		{"/deploys", ""},                   // cross-project lead view, not a workspace
		{"/projects/proj/deploys", "proj"}, // its per-project sibling still scopes
		{"/projects/proj/actions", "proj"},
		{"/projects/proj", "proj"},
		{"/projects/proj/ship", "proj"},
		{"/projects/proj/cases", "proj"},
		{"/projects/proj/sessions", "proj"},
		{"/config/projects/proj", "proj"},
		{"/sessions/thread-99?project=proj", "proj"},
		{"/sessions/thread-99", ""},
		{"/sessions/thread-99?project=nope", ""}, // unknown → global shell
		{"/history/thread-99?project=proj", "proj"},
		{"/history/thread-99", ""},
		{"/history/thread-99?project=nope", ""}, // unknown → global shell
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.path, w.Code, w.Body.String())
		}
		want := `data-scope="` + tc.scope + `"`
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s missing %q", tc.path, want)
		}
	}

	// Session ↔ turn log must keep ?project= so the workspace shell stays put.
	req := httptest.NewRequest(http.MethodGet, "/sessions/thread-99?project=proj", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`href="/history/thread-99?project=proj"`,
		`href="/projects/proj/sessions">← Sessions</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scoped session detail missing %q: %s", want, body)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/history/thread-99?project=proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	histBody := w.Body.String()
	for _, want := range []string{
		`data-scope="proj"`,
		`href="/projects/proj/sessions">← Sessions</a>`,
		`href="/sessions/thread-99?project=proj"`,
		`class="active">Sessions</a>`,
	} {
		if !strings.Contains(histBody, want) {
			t.Fatalf("scoped turn log missing %q", want)
		}
	}
	// Turn log without ?project= still backs to the thread's project sessions
	// (History is not a primary nav tab).
	req = httptest.NewRequest(http.MethodGet, "/history/thread-99", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if hist := w.Body.String(); !strings.Contains(hist, `href="/projects/proj/sessions">← Sessions</a>`) {
		t.Fatalf("unscoped turn log back link missing project sessions: %s", hist)
	}

	// Unknown project workspace pages are forbidden, not silently global.
	req = httptest.NewRequest(http.MethodGet, "/projects/nope", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown workspace status=%d want 403", w.Code)
	}

	// Retired feature-first hubs redirect to the launcher.
	for _, path := range []string{"/issues", "/commits"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
			t.Fatalf("%s status=%d want redirect", path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Fatalf("%s Location=%q want /", path, loc)
		}
	}
}

// TestProjectScopedBackButtons pins ← back links on detail surfaces to the
// project the user was browsing (NavProject) or the unit's own project.
func TestProjectScopedBackButtons(t *testing.T) {
	srv, _, _ := testServer(t)
	if err := srv.history.Append("hist-only", history.Turn{
		User: "x", Prompt: "p", Response: "r", Status: "done", Project: "proj",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("no-proj", sessionstore.Entry{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	cases := []struct {
		path string
		want string
	}{
		{"/sessions/thread-99", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/sessions/thread-99?project=proj", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/sessions/hist-only", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/sessions/hist-only?project=proj", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/sessions/no-proj?project=proj", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/sessions/no-proj", `href="/sessions">← Sessions</a>`},
		{"/history/thread-99", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/history/thread-99?project=proj", `href="/projects/proj/sessions">← Sessions</a>`},
		{"/history/hist-only", `href="/projects/proj/sessions">← Sessions</a>`},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s status=%d", tc.path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s missing %q", tc.path, tc.want)
		}
	}
}

// TestShipPartialScopedLayout pins SSE-refresh layout parity: workspace ship
// regions refresh with &scoped=1 and must keep the Project column hidden;
// the global board filtered by ?project= must keep the column.
func TestShipPartialScopedLayout(t *testing.T) {
	srv, _, _ := testServer(t)
	// A tracked PR so the fragments render the table, not the empty state.
	if err := srv.sessions.Set("thread-99", sessionstore.Entry{
		SessionID: "sess-99",
		Project:   "proj",
		PRs: []sessionstore.TrackedPR{{
			URL:    "https://github.com/acme/proj/pull/7",
			Number: 7,
			State:  "OPEN",
			Title:  "add feature x",
			Owner:  "acme",
			Repo:   "proj",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		return w.Body.String()
	}

	if body := get("/partials/ship/table?project=proj&state=open"); !strings.Contains(body, "<th>Project</th>") {
		t.Fatal("global ship partial must keep the Project column")
	}
	if body := get("/partials/ship/table?project=proj&state=open&scoped=1"); strings.Contains(body, "<th>Project</th>") {
		t.Fatal("scoped ship partial must hide the Project column")
	}
	// The workspace page must emit scoped refresh URLs.
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/ship", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, "scoped=1") {
		t.Fatal("workspace ship page missing scoped=1 on live-region URLs")
	}
	// Worktrees partial parity: ?project= scopes both data and layout
	// (the worktrees page has no cross-project filter UI to collide with).
	if body := get("/partials/worktrees/table?project=proj"); strings.Contains(body, "<th>Project</th>") {
		t.Fatal("scoped worktrees partial must hide the Project column")
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
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ship page missing %q in %s", want, body)
		}
	}
	// The board is PR-first: rows lead to PR detail, which carries the jump
	// to the session. No Thread column, so no raw unit ids on the board.
	if strings.Contains(body, "<th>Thread</th>") {
		t.Fatal("ship board still renders the Thread column")
	}
	if strings.Contains(body, "thread-99") {
		t.Fatalf("ship board still leaks the unit id in %s", body)
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

// TestCasesBoard pins the support case board: phase lanes with plain-language
// labels, severity ordering, closed hidden by default, filters, and rows
// linking into the session workspace.
func TestCasesBoard(t *testing.T) {
	srv, _, _ := testServer(t)
	seed := map[string]sessionstore.Entry{
		"case-intake": {
			SessionID: "cs1", Project: "proj", Mode: "case", Phase: "intake",
			Severity: "critical", CustomerTitle: "EU checkout returns 500",
			CustomerRef: "ZD-4821", ReporterName: "beam", Origin: "discord",
		},
		"case-investigate": {
			SessionID: "cs2", Project: "proj", Mode: "case", Phase: "investigate",
			Severity: "high", CustomerTitle: "Webhook retries duplicated",
			OwnerName: "mint", CustomerUpdate: "We are reproducing the duplicate retries now.",
		},
		"case-shipping": {
			SessionID: "cs3", Project: "proj", Mode: "case", Phase: "shipping",
			Severity: "medium", CustomerTitle: "Rate limit header missing",
			PRs: []sessionstore.TrackedPR{{
				URL: "https://github.com/acme/proj/pull/12", Number: 12,
				State: "OPEN", Checks: "✓ 2 · ✗ 1", Owner: "acme", Repo: "proj",
			}},
		},
		"case-closed": {
			SessionID: "cs4", Project: "proj", Mode: "case", Phase: "closed",
			CustomerTitle: "Resolved ticket", Resolution: "fixed",
		},
	}
	for id, e := range seed {
		if err := srv.sessions.Set(id, e); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.Handler()
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	body := get("/projects/proj/cases")
	for _, want := range []string{
		`id="page-cases"`,
		// Nav: Cases is the active workspace tab (bare-label contract).
		`class="active">Cases</a>`,
		// Pipeline stages with plain-language sublabels.
		`id="case-pipeline"`,
		"New case", "Looking into it", "Answer ready", "With engineering", "Fix in review", "Resolved",
		// Phase lanes in pipeline order with case rows.
		`id="lane-intake"`, `id="lane-investigate"`, `id="lane-shipping"`,
		"EU checkout returns 500", "Webhook retries duplicated", "Rate limit header missing",
		"ZD-4821", "reporter beam", "owner mint",
		`class="case-row sev-critical"`,
		// Support-facing customer update snippet.
		"customer update", "We are reproducing the duplicate retries now.",
		// Rows open the session workspace, stamping this board as the detail
		// page's ← crumb target; escalated case links its PR.
		`href="/sessions/case-intake?project=proj&amp;back=%2Fprojects%2Fproj%2Fcases"`,
		`href="/prs/acme/proj/12?project=proj"`,
		"CI failing",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cases board missing %q", want)
		}
	}
	// Closed cases are hidden from the default open view.
	if strings.Contains(body, "Resolved ticket") || strings.Contains(body, `id="lane-closed"`) {
		t.Fatal("default view must hide closed cases")
	}

	// scope=all shows the closed lane with its resolution badge.
	all := get("/projects/proj/cases?scope=all")
	for _, want := range []string{`id="lane-closed"`, "Resolved ticket", `status-done">fixed`} {
		if !strings.Contains(all, want) {
			t.Fatalf("scope=all missing %q", want)
		}
	}

	// Phase filter narrows lanes; pipeline counts stay project-wide.
	investigate := get("/projects/proj/cases?phase=investigate")
	if !strings.Contains(investigate, `id="lane-investigate"`) || strings.Contains(investigate, `id="lane-intake"`) {
		t.Fatal("phase filter should show only the investigate lane")
	}
	// Severity filter drops non-matching rows.
	critical := get("/projects/proj/cases?severity=critical")
	if !strings.Contains(critical, "EU checkout returns 500") || strings.Contains(critical, "Webhook retries duplicated") {
		t.Fatal("severity filter should keep only critical cases")
	}

	// Partials carry the same filters and stay content-only.
	req := httptest.NewRequest(http.MethodGet, "/partials/cases/list?project=proj&phase=investigate", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	partial := w.Body.String()
	if !strings.Contains(partial, "Webhook retries duplicated") || strings.Contains(partial, "EU checkout returns 500") {
		t.Fatalf("filtered partial wrong rows: %s", partial)
	}
	if strings.Contains(partial, "<nav") || strings.Contains(partial, "sse-status") {
		t.Fatal("cases partial leaked layout chrome")
	}

	// Unknown project is forbidden, mirroring the other workspace pages.
	req = httptest.NewRequest(http.MethodGet, "/projects/nope/cases", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown project cases status=%d want 403", w.Code)
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
				if !cfg.AccessAllowed("proj", "user-added") {
					t.Fatal("runtime user missing")
				}
			},
		},
		{
			path: "/config/projects/teams",
			form: url.Values{"name": {"proj"}, "key": {"eng"}, "label": {"Engineering"}, "capabilities": {"builder"}},
			check: func(t *testing.T) {
				if _, ok := cfg.ProjectTeams("proj")["eng"]; !ok {
					t.Fatalf("runtime team missing: %+v", cfg.ProjectTeams("proj"))
				}
			},
		},
		{
			path: "/config/projects/teams/members",
			form: url.Values{"name": {"proj"}, "key": {"eng"}, "id": {"team-added"}},
			check: func(t *testing.T) {
				// Team membership is the grant: access AND the team's template.
				if !cfg.AccessAllowed("proj", "team-added") {
					t.Fatal("runtime team member missing")
				}
				if !cfg.ResolveCapabilities("proj", "team-added").CanShip() {
					t.Fatal("team member did not inherit the builder template")
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
		"added", newProj, "Run limits", "Crash-safe active runs", "resumeActiveRuns",
		"CI triage", "Discord PR links", "Completion risk paths", "Channel map",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("config hub missing %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/config/channels", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	chBody := w.Body.String()
	for _, want := range []string{"ch-added", "Remove", "Add channel map"} {
		if !strings.Contains(chBody, want) {
			t.Fatalf("channels page missing %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/config/projects/proj", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	pbody := w.Body.String()
	for _, want := range []string{"user-added", "team-added", "Engineering", "Members", `id="team-roster"`} {
		if !strings.Contains(pbody, want) {
			t.Fatalf("project config missing %q", want)
		}
	}

	// Settings: Grok run limits (unit suffix, e.g. 20m)
	reqRun := httptest.NewRequest(http.MethodPost, "/config/run", strings.NewReader(url.Values{
		"maxTurns":  {"55"},
		"timeoutMs": {"20m"},
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
	// Bare milliseconds still accepted for backward compatibility.
	reqRunMs := httptest.NewRequest(http.MethodPost, "/config/run", strings.NewReader(url.Values{
		"maxTurns":  {"55"},
		"timeoutMs": {"900000"},
	}.Encode()))
	reqRunMs.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRunMs := httptest.NewRecorder()
	h.ServeHTTP(wRunMs, reqRunMs)
	if wRunMs.Code != http.StatusSeeOther && wRunMs.Code != http.StatusFound {
		t.Fatalf("run settings ms status=%d body=%s", wRunMs.Code, wRunMs.Body.String())
	}
	if cfg.TimeoutMsValue() != 900_000 {
		t.Fatalf("run limits bare-ms timeout=%d", cfg.TimeoutMsValue())
	}

	// Settings: concurrency caps round-trip through the same run-limits form.
	reqConc := httptest.NewRequest(http.MethodPost, "/config/run", strings.NewReader(url.Values{
		"maxTurns":              {"55"},
		"timeoutMs":             {"900000"},
		"maxConcurrentRuns":     {"3"},
		"maxConcurrentRunsUser": {"1"},
	}.Encode()))
	reqConc.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wConc := httptest.NewRecorder()
	h.ServeHTTP(wConc, reqConc)
	if wConc.Code != http.StatusSeeOther && wConc.Code != http.StatusFound {
		t.Fatalf("concurrency settings status=%d body=%s", wConc.Code, wConc.Body.String())
	}
	if cfg.MaxConcurrentRunsValue() != 3 {
		t.Fatalf("MaxConcurrentRunsValue=%d want 3", cfg.MaxConcurrentRunsValue())
	}
	if cfg.MaxConcurrentRunsUserValue() != 1 {
		t.Fatalf("MaxConcurrentRunsUserValue=%d want 1", cfg.MaxConcurrentRunsUserValue())
	}

	// The page reflects the saved caps in the number inputs.
	reqRunPage := httptest.NewRequest(http.MethodGet, "/config/run", nil)
	wRunPage := httptest.NewRecorder()
	h.ServeHTTP(wRunPage, reqRunPage)
	runBody := wRunPage.Body.String()
	for _, want := range []string{`name="maxConcurrentRuns" min="0" step="1" value="3"`, `name="maxConcurrentRunsUser" min="0" step="1" value="1"`} {
		if !strings.Contains(runBody, want) {
			t.Fatalf("run page missing %q in body:\n%s", want, runBody)
		}
	}

	// Empty and "0" both mean unlimited and must persist as nil (not a pointer
	// to 0), matching the *int tri-state pattern used elsewhere in config.
	reqUnlimited := httptest.NewRequest(http.MethodPost, "/config/run", strings.NewReader(url.Values{
		"maxTurns":              {"55"},
		"timeoutMs":             {"900000"},
		"maxConcurrentRuns":     {""},
		"maxConcurrentRunsUser": {"0"},
	}.Encode()))
	reqUnlimited.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wUnlimited := httptest.NewRecorder()
	h.ServeHTTP(wUnlimited, reqUnlimited)
	if wUnlimited.Code != http.StatusSeeOther && wUnlimited.Code != http.StatusFound {
		t.Fatalf("unlimited concurrency status=%d body=%s", wUnlimited.Code, wUnlimited.Body.String())
	}
	if cfg.MaxConcurrentRunsValue() != 0 {
		t.Fatalf("MaxConcurrentRunsValue=%d want 0 (unlimited)", cfg.MaxConcurrentRunsValue())
	}
	if cfg.MaxConcurrentRunsUserValue() != 0 {
		t.Fatalf("MaxConcurrentRunsUserValue=%d want 0 (unlimited)", cfg.MaxConcurrentRunsUserValue())
	}
	if cfg.MaxConcurrentRuns != nil {
		t.Fatalf("MaxConcurrentRuns must be stored nil for unlimited, got %v", *cfg.MaxConcurrentRuns)
	}
	if cfg.MaxConcurrentRunsUser != nil {
		t.Fatalf("MaxConcurrentRunsUser must be stored nil for unlimited, got %v", *cfg.MaxConcurrentRunsUser)
	}

	// The page falls back to an empty (placeholder-driven) box once unlimited.
	reqRunPage2 := httptest.NewRequest(http.MethodGet, "/config/run", nil)
	wRunPage2 := httptest.NewRecorder()
	h.ServeHTTP(wRunPage2, reqRunPage2)
	runBody2 := wRunPage2.Body.String()
	for _, want := range []string{`name="maxConcurrentRuns" min="0" step="1" value=""`, `name="maxConcurrentRunsUser" min="0" step="1" value=""`} {
		if !strings.Contains(runBody2, want) {
			t.Fatalf("run page missing %q after clearing caps, body:\n%s", want, runBody2)
		}
	}

	// Rejects a negative cap outright instead of silently clamping it, and
	// must not half-save the rest of the same form (maxTurns here changes
	// from 55 to 91, which must NOT stick when the cap field is invalid).
	reqNeg := httptest.NewRequest(http.MethodPost, "/config/run", strings.NewReader(url.Values{
		"maxTurns":              {"91"},
		"timeoutMs":             {"900000"},
		"maxConcurrentRuns":     {"-1"},
		"maxConcurrentRunsUser": {""},
	}.Encode()))
	reqNeg.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wNeg := httptest.NewRecorder()
	h.ServeHTTP(wNeg, reqNeg)
	if wNeg.Code != http.StatusSeeOther && wNeg.Code != http.StatusFound {
		t.Fatalf("negative concurrency status=%d body=%s", wNeg.Code, wNeg.Body.String())
	}
	loc := wNeg.Result().Header.Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("expected redirect with err= for negative cap, got %q", loc)
	}
	if cfg.MaxConcurrentRuns != nil {
		t.Fatalf("negative cap must not have been persisted, got %v", *cfg.MaxConcurrentRuns)
	}
	if cfg.MaxTurnsValue() != 55 {
		t.Fatalf("maxTurns must not have been half-saved alongside the rejected cap, got %d", cfg.MaxTurnsValue())
	}

	// Settings: default coding agent
	reqAgent := httptest.NewRequest(http.MethodPost, "/config/agent", strings.NewReader(url.Values{
		"agent":                     {"grok"},
		"claudeBin":                 {"/opt/claude"},
		"model":                     {"claude-opus-5"},
		"summarizeModel":            {"claude-haiku-4-5"},
		"claudeIncludeAnthropicEnv": {"1"},
	}.Encode()))
	reqAgent.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wAgent := httptest.NewRecorder()
	h.ServeHTTP(wAgent, reqAgent)
	if wAgent.Code != http.StatusSeeOther && wAgent.Code != http.StatusFound {
		t.Fatalf("agent settings status=%d body=%s", wAgent.Code, wAgent.Body.String())
	}
	// The model name selects the CLI: "claude-opus-5" routes to claude even though
	// the fallback agent is grok.
	agentCLI := cfg.ResolveAgentCLI("")
	if agentCLI.Agent != grokrun.AgentClaude || agentCLI.Bin != "/opt/claude" || agentCLI.Model != "claude-opus-5" {
		t.Fatalf("agent settings not applied: %+v", agentCLI)
	}
	if !cfg.AgentIncludesAnthropicEnv(grokrun.AgentClaude) {
		t.Fatal("ANTHROPIC_* opt-in not persisted")
	}
	// Thread titles run on the cheap model; tasks keep the full one.
	if got := cfg.ResolveSummarizeCLI("").Model; got != "claude-haiku-4-5" {
		t.Fatalf("title model=%q", got)
	}
	// A model that is not one of the offered options must not be persisted — the
	// dropdown is the whole allowed set, so a hand-crafted POST cannot widen it.
	reqBadModel := httptest.NewRequest(http.MethodPost, "/config/agent", strings.NewReader(url.Values{
		"agent": {"grok"}, "model": {"gpt-5"},
	}.Encode()))
	reqBadModel.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), reqBadModel)
	if got := cfg.ResolveAgentCLI("").Model; got != "claude-opus-5" {
		t.Fatalf("rejected model overwrote config: %q", got)
	}
	// An unknown agent is rejected instead of silently falling back.
	reqBadAgent := httptest.NewRequest(http.MethodPost, "/config/agent", strings.NewReader(url.Values{
		"agent": {"gpt"},
	}.Encode()))
	reqBadAgent.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wBadAgent := httptest.NewRecorder()
	h.ServeHTTP(wBadAgent, reqBadAgent)
	if got := cfg.ResolveAgentCLI("").Agent; got != grokrun.AgentClaude {
		t.Fatalf("rejected agent overwrote config: %q", got)
	}

	// Settings: worktree dir + idle TTL
	customWT := filepath.Join(t.TempDir(), "custom-worktrees")
	reqTTL := httptest.NewRequest(http.MethodPost, "/config/worktrees", strings.NewReader(url.Values{
		"worktreeIdleTTLDays": {"14"}, "terminalSessionTTLDays": {"0"},
		"worktreeDir": {customWT},
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
	if cfg.WorktreeDirValue() != customWT {
		t.Fatalf("worktreeDir=%q want %q", cfg.WorktreeDirValue(), customWT)
	}
	if cfg.WorktreesRoot() != filepath.Clean(customWT) {
		t.Fatalf("WorktreesRoot=%q want %q", cfg.WorktreesRoot(), filepath.Clean(customWT))
	}

	// Settings: CI triage
	reqCI := httptest.NewRequest(http.MethodPost, "/config/ci", strings.NewReader(url.Values{
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
	reqRisk := httptest.NewRequest(http.MethodPost, "/config/risky", strings.NewReader(url.Values{
		"riskyPathGlobs":      {"**/auth/**\n**/deploy/**"},
		"riskyPathUseDefault": {""},
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
	reqBoard := httptest.NewRequest(http.MethodPost, "/config/board", strings.NewReader(url.Values{
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

	// Settings: Discord PR link mode
	reqPRLink := httptest.NewRequest(http.MethodPost, "/config/pr-links", strings.NewReader(url.Values{
		"discordPRLink": {"web"},
	}.Encode()))
	reqPRLink.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wPRLink := httptest.NewRecorder()
	h.ServeHTTP(wPRLink, reqPRLink)
	if wPRLink.Code != http.StatusSeeOther && wPRLink.Code != http.StatusFound {
		t.Fatalf("discordPRLink settings status=%d body=%s", wPRLink.Code, wPRLink.Body.String())
	}
	if cfg.DiscordPRLinkValue() != config.DiscordPRLinkWeb {
		t.Fatalf("discordPRLink=%q", cfg.DiscordPRLinkValue())
	}

	// Removes
	removes := []postCase{
		{
			path: "/config/projects/users/remove",
			form: url.Values{"name": {"proj"}, "id": {"user-added"}},
			check: func(t *testing.T) {
				if cfg.AccessAllowed("proj", "user-added") {
					t.Fatal("user still allowed")
				}
			},
		},
		{
			path: "/config/projects/teams/members/remove",
			form: url.Values{"name": {"proj"}, "key": {"eng"}, "id": {"team-added"}},
			check: func(t *testing.T) {
				if cfg.AccessAllowed("proj", "team-added") {
					t.Fatal("team member still allowed")
				}
			},
		},
		{
			path: "/config/projects/teams/remove",
			form: url.Values{"name": {"proj"}, "key": {"eng"}},
			check: func(t *testing.T) {
				if _, ok := cfg.ProjectTeams("proj")["eng"]; ok {
					t.Fatalf("team still present: %+v", cfg.ProjectTeams("proj"))
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
		Channels       map[string]string  `json:"channels"`
		AllowedUserIDs []string           `json:"allowedUserIds"`
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
	if _, ok := disk.Projects["proj"].Teams["eng"]; ok {
		t.Fatalf("disk still has removed team: %+v", disk.Projects["proj"].Teams)
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

	// Save repos → back to the project's Integrations tab with flash.
	form := url.Values{"name": {"proj"}, "repos": {"acme/app\nacme/api"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/github", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set repos status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj/integrations?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("set repos Location=%q", loc)
	}

	// Channel map from the project page round-trips to Integrations.
	form = url.Values{"channelId": {"ch-proj-2"}, "project": {"proj"}, "return_to": {"project"}}
	req = httptest.NewRequest(http.MethodPost, "/config/channels", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj/integrations?") {
		t.Fatalf("add channel Location=%q", loc)
	}
	if p, ok := cfg.ChannelProject("ch-proj-2"); !ok || p != "proj" {
		t.Fatalf("channel not mapped: %q %v", p, ok)
	}

	// Repo fetch interval save.
	form = url.Values{"name": {"proj"}, "repoFetchIntervalMinutes": {"15"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/fetch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set fetch status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg.ProjectRepoFetchIntervalMinutes("proj") != 15 {
		t.Fatalf("fetch interval=%d", cfg.ProjectRepoFetchIntervalMinutes("proj"))
	}

	// Team policy (safe team) posts alone from the Access tab.
	form = url.Values{
		"name":                    {"proj"},
		"safeTeamMode":            {"1"},
		"safeTeamDefaultTemplate": {"investigator"},
	}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/safe-team", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set safe-team status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj?") {
		t.Fatalf("set safe-team Location=%q", loc)
	}
	if !cfg.SafeTeamMode("proj") {
		t.Fatal("SafeTeamMode not set")
	}

	// Default mode posts separately from the Workflow tab.
	form = url.Values{"name": {"proj"}, "defaultMode": {"case"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/mode", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set mode status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj/workflow?") {
		t.Fatalf("set mode Location=%q", loc)
	}
	if cfg.ProjectDefaultMode("proj") != "case" {
		t.Fatalf("defaultMode=%q", cfg.ProjectDefaultMode("proj"))
	}

	// Capability map user.
	form = url.Values{"name": {"proj"}, "id": {"u-builder"}, "template": {"builder"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/capabilities/users", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("map user status=%d", w.Code)
	}
	caps := cfg.ResolveCapabilities("proj", "u-builder")
	if !caps.CanShip() {
		t.Fatalf("mapped builder cannot ship: %+v", caps)
	}

	// Verify commands.
	form = url.Values{
		"name":           {"proj"},
		"verifyCommands": {"unit | go test ./...\nlint | make lint | 300000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("set verify status=%d body=%s", w.Code, w.Body.String())
	}
	vc := cfg.ProjectVerifyCommands("proj")
	if len(vc) != 2 || vc[0].Name != "unit" || vc[1].TimeoutMs != 300000 {
		t.Fatalf("verify cmds: %+v", vc)
	}

	// Each tab renders its slice of the saved state.
	pages := []struct {
		path  string
		wants []string
	}{
		{"/config/projects/proj", []string{
			`id="page-project-config"`,
			`id="project-policy"`,
			`name="safeTeamMode"`,
			"checked",
			`id="member-roster"`,
			`id="project-teams"`,
			"u-builder",
			"builder",
			"not on member list", // capability map without allowlist → inert row
		}},
		{"/config/projects/proj/workflow", []string{
			`id="page-project-config-workflow"`,
			`name="directToPrimary"`,
			`id="project-verify"`,
			"go test ./...",
			"make lint",
			// Case id prefix: the control, and the maxlength that has to mirror
			// the server's cap (a longer prefix mints truncated).
			`id="project-case-key"`,
			`name="caseKey"`,
			`maxlength="10"`,
		}},
		{"/config/projects/proj/integrations", []string{
			`id="page-project-config-integrations"`,
			"acme/app",
			"ch-proj-2",
			`name="return_to"`,
			`name="repoFetchIntervalMinutes"`,
			`value="15"`,
		}},
		{"/config/projects/proj/danger", []string{
			`id="page-project-config-danger"`,
			"Remove project",
		}},
	}
	for _, pg := range pages {
		req = httptest.NewRequest(http.MethodGet, pg.path, nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", pg.path, w.Code)
		}
		body := w.Body.String()
		for _, want := range pg.wants {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", pg.path, want)
			}
		}
	}
}

func TestGenerateProjectVerifyDraft(t *testing.T) {
	srv, cfg, _ := testServer(t)
	// Seed existing saved commands so we can prove generate does not overwrite config.
	if err := cfg.SetProjectVerifyCommands("proj", []config.VerifyCommand{
		{Name: "old", Command: "echo old"},
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	var sawActivity bool
	srv.suggestVerify = func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, hooks *grokrun.SuggestStreamHooks) (string, error) {
		called = true
		if cwd == "" {
			t.Fatal("empty cwd")
		}
		if hooks != nil {
			if hooks.OnActivity != nil {
				hooks.OnActivity("read_file: go.mod")
				sawActivity = true
			}
			if hooks.OnTextDelta != nil {
				hooks.OnTextDelta("unit | go test ./...\n")
			}
		}
		return "Here you go:\n\n```\nunit | go test ./...\nlint | make lint | 120000\n```\n", nil
	}
	h := srv.Handler()

	form := url.Values{"name": {"proj"}}
	req := httptest.NewRequest(http.MethodPost, "/config/projects/verify/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("generate status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}
	if !called {
		t.Fatal("suggestVerify not called")
	}
	if !sawActivity {
		t.Fatal("expected activity hook to fire")
	}
	body := w.Body.String()
	for _, want := range []string{
		"event: status",
		"event: activity",
		"event: text",
		"event: result",
		"event: done",
		`"text":"unit | go test ./...\nlint | make lint | 120000"`,
		`"count":2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, body)
		}
	}
	// Config still has the old saved commands.
	if vc := cfg.ProjectVerifyCommands("proj"); len(vc) != 1 || vc[0].Name != "old" {
		t.Fatalf("config mutated before save: %+v", vc)
	}

	// Workflow page shows the draft in the textarea (and survives refresh).
	for i := range 2 {
		req = httptest.NewRequest(http.MethodGet, "/config/projects/proj/workflow", nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("workflow status=%d", w.Code)
		}
		page := w.Body.String()
		if !strings.Contains(page, "unit | go test ./...") || !strings.Contains(page, "make lint") {
			t.Fatalf("draft missing from page (load %d): %s", i+1, page)
		}
		if strings.Contains(page, "echo old") {
			t.Fatal("page still showing saved commands instead of draft")
		}
	}

	// Save the draft → clears pending draft and persists.
	form = url.Values{
		"name":           {"proj"},
		"verifyCommands": {"unit | go test ./...\nlint | make lint | 120000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("save status=%d", w.Code)
	}
	vc := cfg.ProjectVerifyCommands("proj")
	if len(vc) != 2 || vc[0].Name != "unit" || vc[1].TimeoutMs != 120000 {
		t.Fatalf("saved: %+v", vc)
	}
	if srv.peekVerifyDraft("proj") != "" {
		t.Fatal("draft should clear after save")
	}
}

func TestGenerateProjectVerifyError(t *testing.T) {
	srv, _, _ := testServer(t)
	srv.suggestVerify = func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, hooks *grokrun.SuggestStreamHooks) (string, error) {
		return "", context.DeadlineExceeded
	}
	h := srv.Handler()
	form := url.Values{"name": {"proj"}}
	req := httptest.NewRequest(http.MethodPost, "/config/projects/verify/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("want error event: %s", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("want done event: %s", body)
	}
	if !strings.Contains(body, "deadline") && !strings.Contains(body, "Deadline") {
		t.Fatalf("want deadline message: %s", body)
	}
}

func TestProjectMemberRoster(t *testing.T) {
	srv, cfg, _ := testServer(t)
	h := srv.Handler()

	// Add member with an explicit role in one post: allowlist + capability map.
	form := url.Values{"name": {"proj"}, "id": {"u-new"}, "template": {"builder"}}
	req := httptest.NewRequest(http.MethodPost, "/config/projects/members", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("add member status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/config/projects/proj?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("add member Location=%q", loc)
	}
	if !cfg.AccessAllowed("proj", "u-new") {
		t.Fatal("added member not allowlisted")
	}
	if !cfg.ResolveCapabilities("proj", "u-new").CanShip() {
		t.Fatal("added member missing builder template")
	}

	// A member added without a template stays on the default fallback.
	form = url.Values{"name": {"proj"}, "id": {"u-plain"}, "template": {""}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/members", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("add plain member status=%d", w.Code)
	}
	if !cfg.AccessAllowed("proj", "u-plain") {
		t.Fatal("added member not allowlisted")
	}
	snapPlain := cfg.Snapshot()
	for _, p := range snapPlain.Projects {
		if p.Name != "proj" {
			continue
		}
		for _, m := range p.CapabilityByUser {
			if m.ID == "u-plain" {
				t.Fatalf("empty template must not write a capability map: %+v", m)
			}
		}
	}

	// Roster role select posting an empty template resets to default.
	form = url.Values{"name": {"proj"}, "id": {"u-new"}, "template": {""}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/capabilities/users", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("reset role status=%d", w.Code)
	}
	snap := cfg.Snapshot()
	for _, p := range snap.Projects {
		if p.Name != "proj" {
			continue
		}
		for _, m := range p.CapabilityByUser {
			if m.ID == "u-new" {
				t.Fatalf("capability map for u-new not cleared: %+v", m)
			}
		}
	}

	// Removing a member also drops any explicit role (no inert map left).
	if err := cfg.SetProjectCapabilityByUser("proj", "u-new", "approver"); err != nil {
		t.Fatal(err)
	}
	form = url.Values{"name": {"proj"}, "id": {"u-new"}}
	req = httptest.NewRequest(http.MethodPost, "/config/projects/users/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("remove member status=%d", w.Code)
	}
	if cfg.AccessAllowed("proj", "u-new") {
		t.Fatal("removed member still allowlisted")
	}
	snap = cfg.Snapshot()
	for _, p := range snap.Projects {
		if p.Name != "proj" {
			continue
		}
		for _, m := range p.CapabilityByUser {
			if m.ID == "u-new" {
				t.Fatalf("capability map for removed member kept: %+v", m)
			}
		}
	}
}

func TestProjectConfigMemberNames(t *testing.T) {
	srv, cfg, _ := testServer(t)
	// Seed a known member display name via a web login session.
	if _, _, err := srv.LoginAs("u0", "Alice Example", config.WebRoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Another member only known from a past thread owner.
	if err := cfg.AddProjectAllowedUser("proj", "u-owner"); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("t-owner", sessionstore.Entry{
		SessionID: "s-owner", Project: "proj",
		OwnerID: "u-owner", OwnerName: "Owner Bob",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/config/projects/proj", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"u0", "Alice Example",
		"u-owner", "Owner Bob",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("project members missing %q", want)
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
	tr, err := gitworktree.Ensure(context.Background(), repo, cfg.WorktreesRoot(), "proj", threadID)
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
	cfg.WorktreeIdleTTLDays = new(30)
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

// syncRecorder serializes access to an httptest.ResponseRecorder.
//
// ResponseRecorder is not safe for concurrent use, and an SSE handler keeps
// writing for as long as the stream is open. A test that polls Body.String() to
// wait for the first event therefore races every Write in the handler goroutine
// — which is what -race reported against TestSSE.
//
// It must implement http.Flusher by type assertion rather than relying on
// http.ResponseController: Server.sse does `flusher, ok := w.(http.Flusher)` and
// gives up when that fails, so a wrapper without Flush would silently change
// what this test exercises.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{rec: httptest.NewRecorder()}
}

// Header is unguarded: the handler sets headers before starting the write loop,
// and the test only reads them after <-done.
func (s *syncRecorder) Header() http.Header { return s.rec.Header() }

func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(p)
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

func (s *syncRecorder) code() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Code
}

func TestSSE(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	w := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = w.body()
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
	if w.code() != http.StatusOK {
		t.Fatalf("status=%d", w.code())
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
	var hello struct {
		Domain string `json:"domain"`
		Revs   *struct {
			Dashboard string `json:"dashboard"`
			Ship      string `json:"ship"`
			Cases     string `json:"cases"`
			History   string `json:"history"`
			Worktrees string `json:"worktrees"`
			Config    string `json:"config"`
		} `json:"revs"`
		bot.StatusSnapshot
	}
	if err := json.Unmarshal([]byte(payload), &hello); err != nil {
		t.Fatalf("unmarshal payload %q: %v", payload, err)
	}
	if hello.Domain != "hello" {
		t.Fatalf("expected domain=hello got %q", hello.Domain)
	}
	if hello.SessionCount < 1 {
		t.Fatalf("expected sessionCount>=1 got %+v", hello.StatusSnapshot)
	}
	if hello.ProjectCount < 1 {
		t.Fatalf("expected projectCount>=1 got %+v", hello.StatusSnapshot)
	}
	if hello.Time.IsZero() {
		t.Fatal("time zero in SSE payload")
	}
	if hello.Revs == nil {
		t.Fatal("hello missing revs for reconnect catch-up")
	}
	if hello.Revs.Dashboard == "" || hello.Revs.Ship == "" || hello.Revs.Cases == "" ||
		hello.Revs.History == "" || hello.Revs.Worktrees == "" || hello.Revs.Config == "" {
		t.Fatalf("hello revs incomplete: %+v", hello.Revs)
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
