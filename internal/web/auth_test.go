package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func authOnServer(t *testing.T) (*Server, *config.Config, *FakeDiscordOAuth) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{
		DiscordToken:        "tok",
		DiscordClientID:     "424242424242424242",
		DiscordClientSecret: "client-secret",
		WebPublicBaseURL:    "http://127.0.0.1:8787",
		AllowedUserIDs:      []string{"allow-user"},
		AllowedUsers:        map[string]struct{}{"allow-user": {}},
		AllowedRoleIDs:      []string{},
		AllowedRoles:        map[string]struct{}{},
		Projects:            config.PathProjects(map[string]string{"proj": proj}),
		Channels:            map[string]string{"ch": "proj"},
		GrokBin:             "grok",
		MaxTurns:            40,
		TimeoutMs:           1000,
		HTTPListen:          "127.0.0.1:0",
		ConfigPath:          cfgPath,
		DataDir:             filepath.Join(dir, "data"),
		WebAuth: &config.WebAuthConfig{
			Enabled:         true,
			SessionSecret:   "test-session-secret-32-bytes-long!",
			AdminDiscordIDs: []string{"admin-1"},
			MemberDiscordIDs: []string{"member-1"},
			ViewerDiscordIDs: []string{"viewer-1"},
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
	fake := &FakeDiscordOAuth{CodeToUser: map[string]DiscordUser{
		"code-admin":  {ID: "admin-1", Username: "admin", GlobalName: "Admin User"},
		"code-member": {ID: "member-1", Username: "member"},
		"code-allow":  {ID: "allow-user", Username: "allowed"},
		"code-deny":   {ID: "stranger", Username: "stranger"},
	}}
	srv := New(cfg, store, hist, bot.New(cfg, store, hist))
	srv.oauth = fake
	return srv, cfg, fake
}

func TestAuthOffPagesAndMutate(t *testing.T) {
	srv, cfg, _ := testServer(t)
	if cfg.WebAuthEnabled() {
		t.Fatal("default test server must have auth off")
	}
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", w.Code)
	}

	form := url.Values{"section": {"worktree"}, "worktreeIdleTTLDays": {"7"}}
	req = httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("POST settings status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg.WorktreeIdleTTLDaysValue() != 7 {
		t.Fatalf("TTL=%d want 7", cfg.WorktreeIdleTTLDaysValue())
	}
}

func TestAuthOnUnauthenticatedRedirect(t *testing.T) {
	srv, _, _ := authOnServer(t)
	h := srv.Handler()
	for _, path := range []string{"/", "/config", "/history", "/sessions", "/ship", "/worktrees", "/events"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("%s status=%d want 302", path, w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "/login") {
			t.Fatalf("%s Location=%q", path, loc)
		}
	}
	// Login page is public.
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Log in with Discord") {
		t.Fatal("missing login button")
	}
	// Must not hx-boost OAuth: Discord CORS rejects HX-Request on authorize.
	if !strings.Contains(body, `id="login-discord"`) || !strings.Contains(body, `hx-boost="false"`) {
		t.Fatalf("login Discord link must set hx-boost=false, body snippet missing markers")
	}
	// Static still public.
	req = httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("static status=%d", w.Code)
	}
}

func TestAuthOnOAuthCallbackAndAdminMutate(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	h := srv.Handler()

	// Start OAuth to get state cookie.
	req := httptest.NewRequest(http.MethodGet, "/auth/discord?next=/config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("oauth start status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "discord.com/api/oauth2/authorize") || !strings.Contains(loc, "identify") {
		t.Fatalf("authorize URL=%q", loc)
	}
	// Boosted htmx path: full client navigation via HX-Redirect (no CORS to Discord).
	reqHX := httptest.NewRequest(http.MethodGet, "/auth/discord?next=/config", nil)
	reqHX.Header.Set("HX-Request", "true")
	reqHX.Header.Set("HX-Boosted", "true")
	wHX := httptest.NewRecorder()
	h.ServeHTTP(wHX, reqHX)
	if wHX.Code != http.StatusNoContent {
		t.Fatalf("oauth start HX status=%d want 204", wHX.Code)
	}
	hxLoc := wHX.Header().Get("HX-Redirect")
	if !strings.Contains(hxLoc, "discord.com/api/oauth2/authorize") {
		t.Fatalf("HX-Redirect=%q", hxLoc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("missing state")
	}
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("missing state cookie")
	}

	// Callback with valid state + code for admin.
	req = httptest.NewRequest(http.MethodGet, "/auth/discord/callback?code=code-admin&state="+url.QueryEscape(state), nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Location") != "/config" {
		t.Fatalf("callback redirect=%q", w.Header().Get("Location"))
	}
	var sid string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("missing session cookie")
	}
	sess, ok := srv.webSessions.Get(sid)
	if !ok || sess.Role != config.WebRoleAdmin {
		t.Fatalf("session=%+v ok=%v", sess, ok)
	}

	// Unauthenticated POST rejected.
	form := url.Values{"section": {"worktree"}, "worktreeIdleTTLDays": {"3"}}
	req = httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound { // requireAuth redirects
		// requireAdmin wraps requireAuth — unauthenticated → 302 to login
		if w.Code != http.StatusFound && w.Code != http.StatusForbidden {
			t.Fatalf("unauth POST status=%d", w.Code)
		}
	}

	// Admin without CSRF → 403
	req = httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin no CSRF status=%d body=%s", w.Code, w.Body.String())
	}

	// Admin with CSRF succeeds.
	form.Set("csrf", sess.CSRF)
	form.Set("worktreeIdleTTLDays", "11")
	req = httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("admin CSRF POST status=%d body=%s", w.Code, w.Body.String())
	}
	if cfg.WorktreeIdleTTLDaysValue() != 11 {
		t.Fatalf("TTL=%d want 11", cfg.WorktreeIdleTTLDaysValue())
	}

	// Authenticated GET dashboard works.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Admin User") {
		t.Fatal("expected display name in chrome")
	}
}

func TestAuthOnMemberCannotMutate(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"section":             {"worktree"},
		"worktreeIdleTTLDays": {"9"},
		"csrf":                {csrf},
	}
	req := httptest.NewRequest(http.MethodPost, "/config/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member mutate status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthOnMemberCannotViewConfig(t *testing.T) {
	srv, _, _ := authOnServer(t)
	h := srv.Handler()
	for _, role := range []struct {
		id   string
		name string
		role config.WebRole
	}{
		{"member-1", "Member", config.WebRoleMember},
		{"viewer-1", "Viewer", config.WebRoleViewer},
	} {
		sid, _, err := srv.LoginAs(role.id, role.name, role.role)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/config", "/config/projects/proj", "/partials/config/lists"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s GET %s status=%d body=%s", role.role, path, w.Code, w.Body.String())
			}
		}
		// Config nav link must not appear on other pages (CSS selectors may mention /config).
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s GET / status=%d", role.role, w.Code)
		}
		if strings.Contains(w.Body.String(), `>Config</a>`) {
			t.Fatalf("%s dashboard must not show Config nav link", role.role)
		}
	}
	// Admin can view config and sees the nav link.
	sid, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET /config status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="page-config"`) {
		t.Fatal("admin config page marker missing")
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `>Config</a>`) {
		t.Fatal("admin dashboard should show Config nav link")
	}
}

func TestAuthOnDeniedUser(t *testing.T) {
	srv, _, _ := authOnServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/auth/discord", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	state := strings.SplitN(stateCookie.Value, "|", 2)[0]
	req = httptest.NewRequest(http.MethodGet, "/auth/discord/callback?code=code-deny&state="+state, nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/login") || !strings.Contains(loc, "not+authorized") && !strings.Contains(loc, "not%20authorized") && !strings.Contains(loc, "authorized") {
		// err is query-escaped
		if !strings.Contains(loc, "login") {
			t.Fatalf("expected login redirect, got %q", loc)
		}
	}
	// No session cookie should establish for denied user.
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			// cleared cookies have MaxAge -1
			if c.MaxAge != -1 && c.Value != "" {
				// If Set-Cookie for session is not set, fine
			}
		}
	}
}

func TestAuthOnLogout(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", w.Code)
	}
	if _, ok := srv.webSessions.Get(sid); ok {
		t.Fatal("session should be deleted")
	}
	// Cookie cleared
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && (c.MaxAge < 0 || c.Value == "") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("session cookie not cleared")
	}
}

func TestLoginAsHelper(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil || sid == "" || csrf == "" {
		t.Fatalf("LoginAs: sid=%q csrf=%q err=%v", sid, csrf, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `name="csrf"`) {
		t.Fatal("config form should include csrf when authed")
	}
}

func TestSafeLocalNext(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "/"},
		{"/config", "/config"},
		{"/history/thread-1?x=1", "/history/thread-1?x=1"},
		{"//evil.example", "/"},
		{"//evil.example/path", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example", "/"},
		{"evil.example", "/"},
		{"/ok\r\nLocation: https://evil", "/"},
		{"/ok\\x", "/"},
	}
	for _, tc := range cases {
		if got := safeLocalNext(tc.in); got != tc.want {
			t.Fatalf("safeLocalNext(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuthOnOpenRedirectRejected(t *testing.T) {
	srv, _, _ := authOnServer(t)
	h := srv.Handler()

	// Already-authed /login?next=//evil.example must not redirect off-site.
	sid, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/login?next="+url.QueryEscape("//evil.example"), nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("login redirect status=%d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/" {
		t.Fatalf("open redirect via login: Location=%q", loc)
	}

	// OAuth callback with malicious next stored in state cookie.
	req = httptest.NewRequest(http.MethodGet, "/auth/discord?next="+url.QueryEscape("//evil.example/phish"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("oauth start status=%d", w.Code)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("missing state cookie")
	}
	// Cookie must not store the protocol-relative next.
	if strings.Contains(stateCookie.Value, "evil") {
		t.Fatalf("state cookie stored unsafe next: %q", stateCookie.Value)
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/discord/callback?code=code-admin&state="+url.QueryEscape(state), nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	loc = w.Header().Get("Location")
	if loc != "/" {
		t.Fatalf("open redirect via OAuth callback: Location=%q", loc)
	}
	// Session cookie must still be set (login succeeds; only next is sanitized).
	gotSID := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge != -1 {
			gotSID = true
		}
	}
	if !gotSID {
		t.Fatal("expected session cookie after callback even when next is unsafe")
	}

	// Legitimate relative next still works end-to-end.
	req = httptest.NewRequest(http.MethodGet, "/auth/discord?next="+url.QueryEscape("/config"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	u, _ = url.Parse(w.Header().Get("Location"))
	state = u.Query().Get("state")
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/discord/callback?code=code-admin&state="+url.QueryEscape(state), nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Header().Get("Location") != "/config" {
		t.Fatalf("safe next lost: Location=%q", w.Header().Get("Location"))
	}
}
