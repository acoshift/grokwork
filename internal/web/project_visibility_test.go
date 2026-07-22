package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// twoProjectAuthServer builds an auth-on server with public (member-1) and secret
// (not listed for member-1) projects, plus seeded sessions/PRs on both.
func twoProjectAuthServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "public")
	secretPath := filepath.Join(dir, "secret")
	for _, p := range []string{publicPath, secretPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{
		DiscordToken:        "tok",
		DiscordClientID:     "424242424242424242",
		DiscordClientSecret: "client-secret",
		WebPublicBaseURL:    "http://127.0.0.1:8787",
		Projects: config.ProjectsMap{
			"public": {Path: publicPath, AllowedUserIDs: []string{"member-1"}},
			"secret": {Path: secretPath, AllowedUserIDs: []string{"other-user"}},
		},
		Channels:   map[string]string{"ch-pub": "public", "ch-sec": "secret"},
		GrokBin:    "grok",
		MaxTurns:   40,
		TimeoutMs:  1000,
		HTTPListen: "127.0.0.1:0",
		ConfigPath: cfgPath,
		DataDir:    filepath.Join(dir, "data"),
		WebAuth: &config.WebAuthConfig{
			Enabled:          true,
			SessionSecret:    "test-session-secret-32-bytes-long!",
			AdminDiscordIDs:  []string{"admin-1"},
			MemberDiscordIDs: []string{"member-1"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateWebAuth(); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		tid, project, owner, repo string
		n                         int
	}{
		{"th-public", "public", "acme", "public", 1},
		{"th-secret", "secret", "acme", "secret", 2},
	} {
		if err := store.Set(item.tid, sessionstore.Entry{
			SessionID: "sess-" + item.tid,
			Project:   item.project,
			OwnerName: "owner",
			Goal:      "work on " + item.project,
			PRs: []sessionstore.TrackedPR{{
				URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", item.owner, item.repo, item.n),
				Number: item.n,
				State:  "OPEN",
				Title:  item.project + " pr",
				Owner:  item.owner,
				Repo:   item.repo,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := hist.Append(item.tid, history.Turn{
			User: "u", Prompt: "p", Response: "r", Status: "done", Project: item.project,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, store, hist, bot.New(cfg, store, hist))
}

func TestAcrossProjectsHidesUnauthorizedProjects(t *testing.T) {
	srv := twoProjectAuthServer(t)
	for _, item := range []struct {
		tid, project string
	}{
		{"run-public", "public"},
		{"run-secret", "secret"},
	} {
		cancel, err := srv.bot.InjectActiveRunForTest(item.tid, item.project)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cancel)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	home := get("/")
	if !strings.Contains(home, `class="proj-card" href="/projects/public"`) {
		t.Fatalf("home missing public project card: %s", home)
	}
	if strings.Contains(home, `class="proj-card" href="/projects/secret"`) {
		t.Fatalf("home leaked secret project card: %s", home)
	}
	if !strings.Contains(home, "run-public") {
		t.Fatalf("home missing public run: %s", home)
	}
	if strings.Contains(home, "run-secret") {
		t.Fatalf("home leaked secret run: %s", home)
	}

	ship := get("/ship")
	if !strings.Contains(ship, "public pr") && !strings.Contains(ship, "acme/public#1") {
		t.Fatalf("ship missing public PR: %s", ship)
	}
	if strings.Contains(ship, "secret pr") || strings.Contains(ship, "acme/secret#2") || strings.Contains(ship, "th-secret") {
		t.Fatalf("ship leaked secret PR: %s", ship)
	}
	if strings.Contains(ship, `value="secret"`) {
		t.Fatalf("ship project filter lists secret: %s", ship)
	}

	sessions := get("/sessions")
	if !strings.Contains(sessions, "th-public") {
		t.Fatalf("sessions missing public thread: %s", sessions)
	}
	if strings.Contains(sessions, "th-secret") {
		t.Fatalf("sessions leaked secret thread: %s", sessions)
	}

	shipSecret := get("/ship?project=secret&state=all")
	if strings.Contains(shipSecret, "acme/secret#2") || strings.Contains(shipSecret, "th-secret") {
		t.Fatalf("ship?project=secret leaked rows: %s", shipSecret)
	}

	adminSID, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminSID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin home status=%d", w.Code)
	}
	adminHome := w.Body.String()
	if !strings.Contains(adminHome, "run-secret") || !strings.Contains(adminHome, `class="proj-card" href="/projects/secret"`) {
		t.Fatalf("admin home should show secret: %s", adminHome)
	}
}

// TestDetailRoutesEnforceProjectACL ensures knowing a thread/PR id cannot bypass
// project membership (IDOR).
func TestDetailRoutesEnforceProjectACL(t *testing.T) {
	srv := twoProjectAuthServer(t)
	if err := srv.cfg.SetProjectGitHubRepos("public", []config.GitHubRepoRef{{Owner: "acme", Repo: "public"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cfg.SetProjectGitHubRepos("secret", []config.GitHubRepoRef{{Owner: "acme", Repo: "secret"}}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}
	getStatus := func(path string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	getBody := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// Allowed project resources.
	if code := getStatus("/sessions/th-public"); code != http.StatusOK {
		t.Fatalf("session public status=%d", code)
	}
	if code := getStatus("/history/th-public"); code != http.StatusOK {
		t.Fatalf("history public status=%d", code)
	}
	if code := getStatus("/projects/public"); code != http.StatusOK {
		t.Fatalf("project public status=%d", code)
	}

	// Forbidden project resources (IDOR by id).
	for _, path := range []string{
		"/sessions/th-secret",
		"/history/th-secret",
		"/partials/sessions/th-secret",
		"/partials/history/turns/th-secret",
		"/sessions/th-secret/diff",
		"/projects/secret",
		"/projects/secret/start",
		"/projects/secret/ship",
		"/projects/secret/sessions",
		"/projects/secret/worktrees",
		"/projects/secret/issues",
		"/projects/secret/commits",
		"/prs/acme/secret/2",
		"/prs/acme/secret/2?project=secret",
		"/prs/acme/secret/2/diff",
	} {
		code, body := getBody(path)
		if code != http.StatusForbidden {
			t.Fatalf("%s status=%d want 403 body=%s", path, code, body)
		}
		if strings.Contains(body, "work on secret") || strings.Contains(body, "secret pr") {
			t.Fatalf("%s leaked secret content: %s", path, body)
		}
	}

	// Admin can open secret thread.
	adminSID, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/th-secret", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminSID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin session secret status=%d", w.Code)
	}
}
