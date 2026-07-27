package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// --- fixture ---------------------------------------------------------------

// authFixture is an auth-on server with a test double installed for every login
// provider. The doubles are always present; whether a provider is *usable* is
// decided by credentials, which is what mutate adjusts.
type authFixture struct {
	srv     *Server
	cfg     *config.Config
	discord *FakeDiscordOAuth
	google  *FakeGoogleOAuth
	github  *FakeGitHubOAuth
}

// newAuthFixture builds the shared auth-on server. mutate runs before Save and
// ValidateWebAuth, so it can add or strip provider credentials.
func newAuthFixture(t *testing.T, mutate func(*config.Config)) *authFixture {
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
		Projects: config.ProjectsMap{
			// member-1 / viewer-1 must be on the project: web Member role alone
			// does not grant CanAccessProject (project ACL is allowlist/team-based).
			"proj": {Path: proj, AllowedUserIDs: []string{"allow-user", "member-1", "viewer-1"}},
		},
		Channels:   map[string]string{"ch": "proj"},
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
			ViewerDiscordIDs: []string{"viewer-1"},
		},
	}
	if mutate != nil {
		mutate(cfg)
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
	f := &authFixture{
		cfg: cfg,
		discord: &FakeDiscordOAuth{CodeToUser: map[string]DiscordUser{
			"code-admin":  {ID: "admin-1", Username: "admin", GlobalName: "Admin User", Avatar: "abc123def"},
			"code-member": {ID: "member-1", Username: "member"},
			"code-allow":  {ID: "allow-user", Username: "allowed"},
			"code-deny":   {ID: "stranger", Username: "stranger"},
		}},
		google: &FakeGoogleOAuth{CodeToUser: map[string]GoogleUser{
			"g-admin": {Sub: "sub-1", Name: "Google Admin", Email: "admin@example.com", Picture: "https://cdn.example/g.png"},
			// No profile name: display must fall back, never break.
			"g-new": {Sub: "sub-x", Email: "nobody@example.com"},
			// A Google sub that collides with the GitHub user id below.
			"g-999": {Sub: "999", Name: "Google Niner"},
		}},
		github: &FakeGitHubOAuth{CodeToUser: map[string]GitHubUser{
			"h-admin": {ID: 999, Login: "alice", Name: "Alice GH", AvatarURL: "https://avatars.example/999"},
			"h-new":   {ID: 4242, Login: "bob"},
		}},
	}
	f.srv = New(cfg, store, hist, bot.New(cfg, store, hist))
	f.srv.oauth = f.discord
	f.srv.oauthGoogle = f.google
	f.srv.oauthGitHub = f.github
	return f
}

// withProviders is the common mutate: give Google and/or GitHub credentials.
func withProviders(google, github bool) func(*config.Config) {
	return func(cfg *config.Config) {
		p := &config.WebAuthProviders{}
		if google {
			p.Google = &config.OAuthProviderConfig{ClientID: "google-client-id", ClientSecret: "google-secret"}
		}
		if github {
			p.GitHub = &config.OAuthProviderConfig{ClientID: "github-client-id", ClientSecret: "github-secret"}
		}
		cfg.WebAuth.Providers = p
	}
}

// oauthStart drives GET /auth/<key> and returns the recorder plus the state
// cookie it issued (nil when none was set).
func startOAuth(t *testing.T, h http.Handler, key, next string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	path := authStartPath(key)
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var c *http.Cookie
	for _, got := range w.Result().Cookies() {
		if got.Name == oauthStateCookieFor(key) && got.Value != "" {
			c = got
		}
	}
	return w, c
}

// finishOAuth replays a callback with the given state cookie and code.
func finishOAuth(t *testing.T, h http.Handler, key string, state *http.Cookie, code, stateParam string) *httptest.ResponseRecorder {
	t.Helper()
	if stateParam == "" && state != nil {
		stateParam = strings.SplitN(state.Value, "|", 2)[0]
	}
	req := httptest.NewRequest(http.MethodGet,
		authCallbackPath(key)+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(stateParam), nil)
	if state != nil {
		req.AddCookie(state)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// loginVia runs a complete start→callback flow.
func loginVia(t *testing.T, h http.Handler, key, code, next string) *httptest.ResponseRecorder {
	t.Helper()
	_, state := startOAuth(t, h, key, next)
	if state == nil {
		t.Fatalf("%s: no state cookie issued", key)
	}
	return finishOAuth(t, h, key, state, code, "")
}

func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

// --- login page ------------------------------------------------------------

// A button renders for exactly the providers that are fully configured, and
// each one carries ?next= and hx-boost="false" (boosting AJAXes /auth/<p> and
// then follows the provider redirect with HX-Request, which their CORS rejects).
func TestLoginPageRendersOnlyConfiguredProviders(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login?next="+url.QueryEscape("/config"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="login-discord"`, `Log in with Discord`, `href="/auth/discord?next=%2Fconfig"`,
		`id="login-google"`, `Log in with Google`, `href="/auth/google?next=%2Fconfig"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	// GitHub has no credentials → no button at all.
	for _, unwanted := range []string{`id="login-github"`, `Log in with GitHub`, `/auth/github`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("login page must not offer the unconfigured GitHub provider (%q)", unwanted)
		}
	}
	// Every provider anchor opts out of boosting.
	for _, key := range []string{"discord", "google"} {
		marker := `id="login-` + key + `"`
		i := strings.Index(body, marker)
		anchorStart := strings.LastIndex(body[:i], "<a ")
		anchor := body[anchorStart : i+len(marker)]
		if !strings.Contains(anchor, `hx-boost="false"`) {
			t.Errorf("%s anchor must set hx-boost=false: %s", key, anchor)
		}
	}
	if strings.Contains(body, "No login providers are configured") {
		t.Error("misconfiguration notice must not show when providers exist")
	}
}

// Zero usable providers is reachable by misconfiguration (an env-sourced secret
// unset while the process runs) and locks everyone out, so the gate has to
// explain itself rather than render a door with no handle.
func TestLoginPageWithNoProvidersConfigured(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_SECRET", "env-github-secret")
	f := newAuthFixture(t, func(cfg *config.Config) {
		cfg.DiscordClientID = ""
		cfg.DiscordClientSecret = ""
		cfg.DiscordToken = ""
		cfg.WebAuth.Providers = &config.WebAuthProviders{
			GitHub: &config.OAuthProviderConfig{ClientID: "github-client-id"},
		}
	})
	// Boots fine: GitHub is complete via the environment.
	if !f.cfg.OAuthProviderConfigured(config.ActorKindGitHub) {
		t.Fatal("github should be configured from the environment")
	}
	os.Unsetenv("GITHUB_CLIENT_SECRET")

	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="login-`) {
		t.Error("no provider is usable; no login button may render")
	}
	if !strings.Contains(body, "No login providers are configured") {
		t.Errorf("expected the misconfiguration notice, got:\n%s", body)
	}
}

// --- the gate is the route, not the button --------------------------------

func TestUnconfiguredProviderRouteRefusesWithoutExchanging(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_SECRET", "env-github-secret")
	f := newAuthFixture(t, func(cfg *config.Config) {
		cfg.WebAuth.Providers = &config.WebAuthProviders{
			GitHub: &config.OAuthProviderConfig{ClientID: "github-client-id"},
		}
	})
	h := f.srv.Handler()

	// Start while configured, so we hold a state cookie the callback would accept.
	_, state := startOAuth(t, h, config.ActorKindGitHub, "")
	if state == nil {
		t.Fatal("expected a state cookie while GitHub is configured")
	}

	// The credential disappears at runtime.
	os.Unsetenv("GITHUB_CLIENT_SECRET")

	w := finishOAuth(t, h, config.ActorKindGitHub, state, "h-admin", "")
	if w.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("callback Location=%q want /login", loc)
	}
	if sid := sessionCookie(w); sid != "" {
		t.Fatal("unconfigured provider must not establish a session")
	}
	// The whole point: refusal happens before the network.
	if f.github.Exchanges != 0 || f.github.Fetches != 0 {
		t.Fatalf("unconfigured provider exchanged anyway: exchanges=%d fetches=%d",
			f.github.Exchanges, f.github.Fetches)
	}

	// Start also refuses, and mints no state.
	w2, state2 := startOAuth(t, h, config.ActorKindGitHub, "")
	if state2 != nil {
		t.Fatal("unconfigured provider must not mint a state cookie")
	}
	if loc := w2.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("start Location=%q want /login", loc)
	}

	// An unknown provider key is a 404, not a 500 and not a half-flow.
	for _, path := range []string{"/auth/nope", "/auth/nope/callback", "/auth/oidc", "/auth/web/callback"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s status=%d want 404", path, w.Code)
		}
	}
}

// --- actor namespaces ------------------------------------------------------

func TestGoogleLoginNamespacesActor(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(true, false)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "google:sub-1")
	})
	h := f.srv.Handler()

	w := loginVia(t, h, config.ActorKindGoogle, "g-admin", "/config")
	if w.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/config" {
		t.Fatalf("Location=%q want /config", loc)
	}
	sid := sessionCookie(w)
	if sid == "" {
		t.Fatal("missing session cookie")
	}
	sess, _, ok := f.srv.webSessions.Get(sid)
	if !ok {
		t.Fatal("session not stored")
	}
	if sess.DiscordUserID != "google:sub-1" {
		t.Fatalf("actor id=%q want %q", sess.DiscordUserID, "google:sub-1")
	}
	if sess.Role != config.WebRoleAdmin {
		t.Fatalf("role=%q want admin", sess.Role)
	}
	if sess.DisplayName != "Google Admin" || sess.AvatarURL != "https://cdn.example/g.png" {
		t.Fatalf("profile=%q/%q", sess.DisplayName, sess.AvatarURL)
	}
	if _, ok := f.srv.webUsers.Get("google:sub-1"); !ok {
		t.Fatal("durable profile must be keyed by the namespaced actor id")
	}
	if _, ok := f.srv.webUsers.Get("sub-1"); ok {
		t.Fatal("a bare subject must never land in the Discord snowflake key space")
	}
}

// Discord's actor id stays BARE. Sessions on disk, audit rows, allowlists,
// teams and capability maps have keyed on the raw snowflake since before actor
// namespaces existed; namespacing it here would be a silent authorization
// migration that quietly demotes every existing user.
func TestDiscordActorStaysBare(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()
	w := loginVia(t, h, config.ActorKindDiscord, "code-admin", "")
	sid := sessionCookie(w)
	if sid == "" {
		t.Fatalf("missing session cookie (status=%d loc=%q)", w.Code, w.Header().Get("Location"))
	}
	sess, _, ok := f.srv.webSessions.Get(sid)
	if !ok {
		t.Fatal("session not stored")
	}
	if sess.DiscordUserID != "admin-1" {
		t.Fatalf("Discord actor id=%q want bare %q", sess.DiscordUserID, "admin-1")
	}
	if _, ok := f.srv.webUsers.Get("discord:admin-1"); ok {
		t.Fatal("Discord profile must stay under the bare snowflake key")
	}
	if _, ok := f.srv.webUsers.Get("admin-1"); !ok {
		t.Fatal("Discord profile missing under the bare snowflake key")
	}
}

// The GitHub identity is the immutable numeric id, never the login handle: a
// user can rename alice → bob, and the freed "alice" is immediately registrable
// by a stranger who would then inherit every allowlist entry naming it.
func TestGitHubIdentityIsNumericIDNotLogin(t *testing.T) {
	t.Run("login handle is not an identity", func(t *testing.T) {
		f := newAuthFixture(t, func(cfg *config.Config) {
			withProviders(false, true)(cfg)
			cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "github:alice")
		})
		w := loginVia(t, f.srv.Handler(), config.ActorKindGitHub, "h-admin", "")
		if sid := sessionCookie(w); sid != "" {
			t.Fatal("an allowlist naming the GitHub login must not admit anyone")
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "login") {
			t.Fatalf("Location=%q", loc)
		}
	})
	t.Run("numeric id is the identity", func(t *testing.T) {
		f := newAuthFixture(t, func(cfg *config.Config) {
			withProviders(false, true)(cfg)
			cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "github:999")
		})
		w := loginVia(t, f.srv.Handler(), config.ActorKindGitHub, "h-admin", "")
		sid := sessionCookie(w)
		if sid == "" {
			t.Fatalf("expected admission (status=%d loc=%q)", w.Code, w.Header().Get("Location"))
		}
		sess, _, _ := f.srv.webSessions.Get(sid)
		if sess.DiscordUserID != "github:999" || sess.Role != config.WebRoleAdmin {
			t.Fatalf("session actor=%q role=%q", sess.DiscordUserID, sess.Role)
		}
		if sess.DisplayName != "Alice GH" {
			t.Fatalf("display name=%q", sess.DisplayName)
		}
	})
}

// The Google identity is "sub", never the email: an address can be changed by
// its owner, and a corporate domain can re-issue a departed employee's address
// to somebody new.
func TestGoogleIdentityIsSubNotEmail(t *testing.T) {
	t.Run("email is not an identity", func(t *testing.T) {
		f := newAuthFixture(t, func(cfg *config.Config) {
			withProviders(true, false)(cfg)
			cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "google:admin@example.com")
		})
		w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-admin", "")
		if sid := sessionCookie(w); sid != "" {
			t.Fatal("an allowlist naming the Google email must not admit anyone")
		}
	})
	t.Run("sub is the identity", func(t *testing.T) {
		f := newAuthFixture(t, func(cfg *config.Config) {
			withProviders(true, false)(cfg)
			cfg.WebAuth.MemberDiscordIDs = append(cfg.WebAuth.MemberDiscordIDs, "google:sub-1")
		})
		w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-admin", "")
		sid := sessionCookie(w)
		if sid == "" {
			t.Fatalf("expected admission (loc=%q)", w.Header().Get("Location"))
		}
		sess, _, _ := f.srv.webSessions.Get(sid)
		if sess.DiscordUserID != "google:sub-1" || sess.Role != config.WebRoleMember {
			t.Fatalf("session actor=%q role=%q", sess.DiscordUserID, sess.Role)
		}
	})
}

// Subject spaces are independent per issuer, so one namespace per provider is
// load-bearing rather than cosmetic: GitHub user id 999 and a Google "sub" of
// 999 are different people, and a shared namespace (or a bare subject) would
// let either inherit the other's grants.
func TestProviderSubjectSpacesDoNotCrossAdmit(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(true, true)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "google:999")
	})
	h := f.srv.Handler()

	// The Google subject named in the allowlist is admitted...
	w := loginVia(t, h, config.ActorKindGoogle, "g-999", "")
	sid := sessionCookie(w)
	if sid == "" {
		t.Fatalf("google:999 should be admitted (loc=%q)", w.Header().Get("Location"))
	}
	sess, _, _ := f.srv.webSessions.Get(sid)
	if sess.DiscordUserID != "google:999" {
		t.Fatalf("actor id=%q want google:999", sess.DiscordUserID)
	}

	// ...and the identical subject arriving from GitHub is not.
	w = loginVia(t, h, config.ActorKindGitHub, "h-admin", "") // GitHub id 999
	if sid := sessionCookie(w); sid != "" {
		sess, _, _ := f.srv.webSessions.Get(sid)
		t.Fatalf("github id 999 inherited google:999's admin grant as %q", sess.DiscordUserID)
	}
	if !strings.Contains(w.Header().Get("Location"), "login") {
		t.Fatalf("Location=%q", w.Header().Get("Location"))
	}
}

// --- state ----------------------------------------------------------------

func TestOAuthStateIsPerProvider(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, true))
	h := f.srv.Handler()

	// Discord keeps the historical cookie name so a login in flight across a
	// restart still completes.
	_, discordState := startOAuth(t, h, config.ActorKindDiscord, "")
	if discordState == nil || discordState.Name != oauthStateCookie {
		t.Fatalf("discord state cookie=%+v want name %q", discordState, oauthStateCookie)
	}

	// A state minted for Google is simply not present at GitHub's callback.
	_, googleState := startOAuth(t, h, config.ActorKindGoogle, "")
	if googleState == nil {
		t.Fatal("no google state cookie")
	}
	if googleState.Name == oauthStateCookieFor(config.ActorKindGitHub) {
		t.Fatal("google and github must not share a state cookie")
	}
	crossed := googleState.Value
	w := finishOAuth(t, h, config.ActorKindGitHub, googleState, "h-admin", strings.SplitN(crossed, "|", 2)[0])
	if sid := sessionCookie(w); sid != "" {
		t.Fatal("a Google state must not satisfy the GitHub callback")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "missing+OAuth+state+cookie") &&
		!strings.Contains(loc, "missing%20OAuth%20state%20cookie") {
		t.Fatalf("Location=%q want a missing-state refusal", loc)
	}

	// A tampered state value is refused.
	_, ghState := startOAuth(t, h, config.ActorKindGitHub, "")
	w = finishOAuth(t, h, config.ActorKindGitHub, ghState, "h-admin", "not-the-state")
	if sid := sessionCookie(w); sid != "" {
		t.Fatal("mismatched state must not establish a session")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "invalid") {
		t.Fatalf("Location=%q want invalid OAuth state", loc)
	}

	// A missing state cookie is refused.
	w = finishOAuth(t, h, config.ActorKindGoogle, nil, "g-admin", "anything")
	if sid := sessionCookie(w); sid != "" {
		t.Fatal("absent state cookie must not establish a session")
	}
	if f.google.Exchanges != 0 {
		t.Fatalf("state was checked after the exchange: exchanges=%d", f.google.Exchanges)
	}
}

// safeLocalNext still guards every provider's post-login hop.
func TestOAuthNextCannotLeaveTheSite(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(true, true)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "google:sub-1", "github:999")
	})
	h := f.srv.Handler()

	for _, tc := range []struct{ key, code string }{
		{config.ActorKindGoogle, "g-admin"},
		{config.ActorKindGitHub, "h-admin"},
	} {
		_, state := startOAuth(t, h, tc.key, "//evil.example/phish")
		if state == nil {
			t.Fatalf("%s: no state cookie", tc.key)
		}
		if strings.Contains(state.Value, "evil") {
			t.Fatalf("%s: state cookie stored the unsafe next: %q", tc.key, state.Value)
		}
		w := finishOAuth(t, h, tc.key, state, tc.code, "")
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Fatalf("%s: open redirect after login: Location=%q", tc.key, loc)
		}
		if sessionCookie(w) == "" {
			t.Fatalf("%s: login should still succeed; only next is sanitized", tc.key)
		}
		// A legitimate relative next survives.
		_, state = startOAuth(t, h, tc.key, "/config")
		w = finishOAuth(t, h, tc.key, state, tc.code, "")
		if loc := w.Header().Get("Location"); loc != "/config" {
			t.Fatalf("%s: safe next lost: Location=%q", tc.key, loc)
		}
	}
}

// --- denial ----------------------------------------------------------------

// A brand-new provider account is a member of nothing. That must be a clean
// "no access", not a confusing error — and it must name the actor id, which is
// the exact string an admin has to add to an allowlist.
func TestUnknownProviderUserGetsClearDenial(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	h := f.srv.Handler()

	w := loginVia(t, h, config.ActorKindGoogle, "g-new", "")
	if w.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if sid := sessionCookie(w); sid != "" {
		t.Fatal("a user with no membership must get no session")
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/login" {
		t.Fatalf("Location=%q", w.Header().Get("Location"))
	}
	msg := loc.Query().Get("err")
	if !strings.Contains(msg, "not authorized for this Grok Work instance") {
		t.Fatalf("err=%q", msg)
	}
	if !strings.Contains(msg, "google:sub-x") {
		t.Fatalf("denial should name the actor id an admin must allowlist: %q", msg)
	}
	// The provider handshake itself succeeded — this is authorization, not error.
	if f.google.Exchanges != 1 || f.google.Fetches != 1 {
		t.Fatalf("exchanges=%d fetches=%d want 1/1", f.google.Exchanges, f.google.Fetches)
	}
	evs, err := f.srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Action == audit.ActionLoginFail && ev.Actor == "google:sub-x" && !ev.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing login.fail audit for google:sub-x: %+v", evs)
	}
}

// --- table drift -----------------------------------------------------------

func TestAuthRoutesMatchProviderTable(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, true))
	h := f.srv.Handler()
	keys := loginProviderOrder()
	if len(keys) != 3 {
		t.Fatalf("provider order=%v", keys)
	}
	for _, key := range keys {
		if got := f.srv.app.Route("auth." + key); got != authStartPath(key) {
			t.Errorf("route auth.%s=%q want %q", key, got, authStartPath(key))
		}
		if got := f.srv.app.Route("auth." + key + ".callback"); got != authCallbackPath(key) {
			t.Errorf("route auth.%s.callback=%q want %q", key, got, authCallbackPath(key))
		}
		if _, ok := f.srv.provider(key); !ok {
			t.Errorf("no provider adapter for %q", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, authStartPath(key), nil))
		if w.Code == http.StatusNotFound {
			t.Errorf("%s start route is not wired", authStartPath(key))
		}
	}
}

// --- secrets ---------------------------------------------------------------

// Nothing from the credential path may reach a redirect, a rendered page or the
// audit log. auditLogin records the actor and a fixed reason string only.
func TestNoSecretsLeakFromLoginFlow(t *testing.T) {
	const sentinelSecret = "SENTINEL-google-secret-6f21ab"
	const sentinelCode = "SENTINEL-code-9f3a"
	f := newAuthFixture(t, func(cfg *config.Config) {
		cfg.WebAuth.Providers = &config.WebAuthProviders{
			Google: &config.OAuthProviderConfig{ClientID: "google-client-id", ClientSecret: sentinelSecret},
		}
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "google:sub-1")
	})
	f.google.CodeToUser[sentinelCode] = f.google.CodeToUser["g-admin"]
	h := f.srv.Handler()

	var seen []string
	record := func(w *httptest.ResponseRecorder) {
		seen = append(seen, w.Header().Get("Location"), w.Header().Get("HX-Redirect"), w.Body.String())
	}

	// A failed login (unknown code → exchange error) and a successful one.
	_, state := startOAuth(t, h, config.ActorKindGoogle, "")
	record(finishOAuth(t, h, config.ActorKindGoogle, state, "no-such-code", ""))
	record(loginVia(t, h, config.ActorKindGoogle, sentinelCode, "/config"))

	// The login page after both.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	record(w)

	for i, s := range seen {
		if strings.Contains(s, sentinelSecret) {
			t.Errorf("response %d leaked the client secret: %s", i, s)
		}
		if strings.Contains(s, sentinelCode) {
			t.Errorf("response %d leaked the authorization code: %s", i, s)
		}
		if strings.Contains(s, "fake-google-") {
			t.Errorf("response %d leaked an access token: %s", i, s)
		}
	}

	raw, err := os.ReadFile(filepath.Join(f.cfg.DataDir, "audit",
		time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	for _, needle := range []string{sentinelSecret, sentinelCode, "fake-google-"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("audit log leaked %q:\n%s", needle, raw)
		}
	}
	// It did record the login, so this is not a vacuous assertion.
	if !strings.Contains(string(raw), "google:sub-1") {
		t.Fatalf("expected the login audit row, got:\n%s", raw)
	}
}

// --- HTTP clients (httptest, never the network) ----------------------------

func TestHTTPGitHubExchangeRejects200ErrorBody(t *testing.T) {
	// GitHub answers a bad or replayed code with HTTP 200 and an "error" field.
	// Trusting the status leaves access_token empty and the failure resurfaces
	// later as a confusing "profile" error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`)
	}))
	defer srv.Close()
	h := &HTTPGitHubOAuth{tokenURL: srv.URL}
	tok, err := h.ExchangeCode(t.Context(), "code", "http://cb", "id", "secret")
	if err == nil {
		t.Fatalf("expected an error, got token %q", tok)
	}
	if !strings.Contains(err.Error(), "bad_verification_code") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPGitHubExchangeSendsAcceptJSON(t *testing.T) {
	var accept, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		contentType = r.Header.Get("Content-Type")
		io.WriteString(w, `{"access_token":"gho_token"}`)
	}))
	defer srv.Close()
	h := &HTTPGitHubOAuth{tokenURL: srv.URL}
	tok, err := h.ExchangeCode(t.Context(), "code", "http://cb", "id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "gho_token" {
		t.Fatalf("token=%q", tok)
	}
	if accept != "application/json" {
		// Without it GitHub replies form-encoded and the JSON decode silently
		// yields an empty token.
		t.Fatalf("Accept=%q", accept)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type=%q", contentType)
	}
}

func TestTokenExchangeErrorsOmitTheBody(t *testing.T) {
	// A token endpoint can quote the authorization code and client id back in
	// its error body, and this string ends up in a log line.
	const body = "the-code-and-client-id-echoed-back"
	for _, tc := range []struct {
		name string
		call func(url string) error
	}{
		{"github", func(u string) error {
			h := &HTTPGitHubOAuth{tokenURL: u}
			_, err := h.ExchangeCode(t.Context(), "c", "http://cb", "id", "secret")
			return err
		}},
		{"google", func(u string) error {
			h := &HTTPGoogleOAuth{tokenURL: u}
			_, err := h.ExchangeCode(t.Context(), "c", "http://cb", "id", "secret")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, body)
			}))
			defer srv.Close()
			err := tc.call(srv.URL)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), body) {
				t.Fatalf("token error echoed the response body: %v", err)
			}
			if !strings.Contains(err.Error(), "400") {
				t.Fatalf("token error should report the status: %v", err)
			}
		})
	}
}

func TestHTTPGitHubUserRejectsZeroID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-GitHub-Api-Version"); got != githubAPIVersion {
			t.Errorf("X-GitHub-Api-Version=%q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "alice", "name": "Alice"})
	}))
	defer srv.Close()
	h := &HTTPGitHubOAuth{userURL: srv.URL}
	if _, err := h.FetchUser(t.Context(), "tok"); err == nil {
		t.Fatal("a response with no numeric id must not yield an identity")
	}
}

func TestHTTPGitHubUserReadsNumericID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":12345,"login":"alice","name":"Alice","avatar_url":"https://a.example/1"}`)
	}))
	defer srv.Close()
	h := &HTTPGitHubOAuth{userURL: srv.URL}
	u, err := h.FetchUser(t.Context(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.Subject() != "12345" {
		t.Fatalf("subject=%q want 12345", u.Subject())
	}
	if u.DisplayName() != "Alice" {
		t.Fatalf("display=%q", u.DisplayName())
	}
}

func TestHTTPGoogleUserRejectsEmptySub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"email":"a@b.c","name":"A"}`)
	}))
	defer srv.Close()
	h := &HTTPGoogleOAuth{userURL: srv.URL}
	if _, err := h.FetchUser(t.Context(), "tok"); err == nil {
		t.Fatal("userinfo with no sub must not fall back to the email")
	}
}

func TestHTTPGoogleUserReadsSub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization=%q", got)
		}
		io.WriteString(w, `{"sub":"1029","email":"a@b.c","picture":"https://p.example/1"}`)
	}))
	defer srv.Close()
	h := &HTTPGoogleOAuth{userURL: srv.URL}
	u, err := h.FetchUser(t.Context(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.Sub != "1029" {
		t.Fatalf("sub=%q", u.Sub)
	}
	// No name → display falls back to the email.
	if u.DisplayName() != "a@b.c" {
		t.Fatalf("display=%q", u.DisplayName())
	}
}

func TestAuthorizeURLScopes(t *testing.T) {
	g, err := url.Parse(googleAuthorizeURL("cid", "https://x/cb", "st"))
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Query().Get("scope"); got != "openid email profile" {
		t.Fatalf("google scope=%q", got)
	}
	if g.Host != "accounts.google.com" || g.Query().Get("response_type") != "code" ||
		g.Query().Get("state") != "st" || g.Query().Get("redirect_uri") != "https://x/cb" {
		t.Fatalf("google authorize URL=%s", g)
	}

	h, err := url.Parse(githubAuthorizeURL("cid", "https://x/cb", "st"))
	if err != nil {
		t.Fatal(err)
	}
	scope := h.Query().Get("scope")
	if scope != "read:user" {
		t.Fatalf("github scope=%q", scope)
	}
	// user:email would buy nothing — display already falls back to the login —
	// but would hand us a private address to store.
	if strings.Contains(scope, "user:email") {
		t.Fatalf("github must not request user:email: %q", scope)
	}
	if h.Host != "github.com" || h.Query().Get("state") != "st" {
		t.Fatalf("github authorize URL=%s", h)
	}
}

// A provider-supplied avatar is rendered into an <img src> and stored durably,
// so anything that is not an http(s) URL is dropped.
func TestSafeAvatarURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://cdn.example/a.png": "https://cdn.example/a.png",
		"http://cdn.example/a.png":  "http://cdn.example/a.png",
		"  https://x/y  ":           "https://x/y",
		"javascript:alert(1)":       "",
		"data:image/png;base64,AA":  "",
		"":                          "",
	}
	for in, want := range cases {
		if got := safeAvatarURL(in); got != want {
			t.Errorf("safeAvatarURL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestActorIDFor(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, subject, want string }{
		{config.ActorKindDiscord, "424242424242424242", "424242424242424242"},
		{config.ActorKindGoogle, "sub-1", "google:sub-1"},
		{config.ActorKindGitHub, "999", "github:999"},
		{config.ActorKindGoogle, "  sub-2 ", "google:sub-2"},
		{config.ActorKindGitHub, "", ""},
		{config.ActorKindDiscord, "  ", ""},
	}
	for _, tc := range cases {
		if got := actorIDFor(tc.key, tc.subject); got != tc.want {
			t.Errorf("actorIDFor(%q,%q)=%q want %q", tc.key, tc.subject, got, tc.want)
		}
	}
}
