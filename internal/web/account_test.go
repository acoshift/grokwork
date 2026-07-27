package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// --- link flow helpers -------------------------------------------------------

func sessCookie(sid string) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: sid}
}

// csrfFor reads a session's CSRF token off the account page, the way a browser
// does — which also keeps the page and the route from drifting apart.
func csrfFor(t *testing.T, h http.Handler, sid string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(sessCookie(sid))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	m := csrfInputRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no csrf input on /account (status=%d)", w.Code)
	}
	return m[1]
}

var csrfInputRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// postLinkStart drives POST /account/link with the given form values.
func postLinkStart(t *testing.T, h http.Handler, sid, csrf, provider string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"provider": {provider}}
	if csrf != "" {
		form.Set("csrf", csrf)
	}
	req := httptest.NewRequest(http.MethodPost, "/account/link", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sid != "" {
		req.AddCookie(sessCookie(sid))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// startLink starts a link flow as the given session and returns the link state
// cookie it issued (nil when none was). It looks the session's CSRF token up
// rather than taking it, so callers keep reading as "this session starts a link".
func startLink(t *testing.T, h http.Handler, key, sid string) *http.Cookie {
	t.Helper()
	csrf := ""
	if sid != "" {
		csrf = csrfFor(t, h, sid)
	}
	w := postLinkStart(t, h, sid, csrf, key)
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthLinkStateCookieFor(key) && c.Value != "" {
			return c
		}
	}
	return nil
}

// finishLink replays the provider callback with a link state cookie and,
// optionally, a session cookie — the two halves the binding compares.
func finishLink(t *testing.T, h http.Handler, key string, state *http.Cookie, code, sid string) *httptest.ResponseRecorder {
	t.Helper()
	stateParam := ""
	if state != nil {
		stateParam, _, _ = strings.Cut(state.Value, "|")
	}
	req := httptest.NewRequest(http.MethodGet,
		authCallbackPath(key)+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(stateParam), nil)
	if state != nil {
		req.AddCookie(state)
	}
	if sid != "" {
		req.AddCookie(sessCookie(sid))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// linkVia runs a whole start→callback link flow for one session.
func linkVia(t *testing.T, f *authFixture, key, code, sid string) *httptest.ResponseRecorder {
	t.Helper()
	state := startLink(t, f.srv.Handler(), key, sid)
	if state == nil {
		t.Fatalf("%s: no link state cookie issued", key)
	}
	return finishLink(t, f.srv.Handler(), key, state, code, sid)
}

func lastAudit(t *testing.T, f *authFixture, action string) audit.Event {
	t.Helper()
	evs, err := f.srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Action == action {
			return evs[i]
		}
	}
	t.Fatalf("no %s audit row in %+v", action, evs)
	return audit.Event{}
}

// savedConfig re-reads config.json from disk. Absorbing that only happened in
// memory is undone by the next restart, at which point the person silently
// loses the access the link was supposed to preserve.
func savedConfig(t *testing.T, f *authFixture) *config.Config {
	t.Helper()
	raw, err := os.ReadFile(f.cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := &config.Config{}
	if err := json.Unmarshal(raw, saved); err != nil {
		t.Fatalf("config.json: %v\n%s", err, raw)
	}
	return saved
}

func redirectErr(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location=%q: %v", w.Header().Get("Location"), err)
	}
	return loc.Query().Get("err")
}

// --- the CSRF binding --------------------------------------------------------

// The single most important refusal in the slice.
//
// Without it, linking is an account-takeover primitive: the attacker starts a
// link flow for THEIR GitHub, lures a signed-in victim into loading the
// callback, and their login is attached to the victim's account — from then on
// they log in as the victim, with the victim's grants. The state cookie alone
// does not help, because in that attack the attacker supplies the state.
func TestLinkRefusedWhenCallbackSessionIsNotTheInitiator(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, nil)
	h := f.srv.Handler()

	attacker, _, err := f.srv.LoginAs("attacker-1", "Attacker", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	victim, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	// The attacker starts the flow; the callback lands in the victim's browser.
	state := startLink(t, h, config.ActorKindGitHub, attacker)
	if state == nil {
		t.Fatal("no link state cookie")
	}
	w := finishLink(t, h, config.ActorKindGitHub, state, "h-admin", victim)

	if got := f.srv.identity.Canonical("github:999"); got != "github:999" {
		t.Fatalf("github:999 was linked to %q — account takeover", got)
	}
	if len(f.srv.identity.AliasesOf("admin-1")) != 0 {
		t.Fatal("the victim's account gained a login it never asked for")
	}
	if msg := redirectErr(t, w); !strings.Contains(msg, "different session") {
		t.Fatalf("err=%q want a session-binding refusal", msg)
	}
	// Refused before the code was redeemed: the attacker's code must not even
	// be spent, so the check has to precede the exchange.
	if f.github.Exchanges != 0 || f.github.Fetches != 0 {
		t.Fatalf("exchanges=%d fetches=%d — the binding was checked after the network call",
			f.github.Exchanges, f.github.Fetches)
	}
	ev := lastAudit(t, f, audit.ActionIdentityLink)
	if ev.OK {
		t.Fatalf("refusal was audited as a success: %+v", ev)
	}
}

// Starting a link is a MUTATION and must be CSRF-protected.
//
// As a GET it was a one-click account merge: a cross-site top-level navigation
// carries the SameSite=Lax session cookie, so the state cookie was minted for the
// victim's own session; the provider re-authorizes an already-approved app with no
// prompt and redirects back — another top-level navigation carrying both Lax
// cookies — so verifyLinkSession matched and the link plus its absorb completed
// with no further interaction. That rewrites config.json and sessions.json, and
// unlinking does not put them back. On a shared machine (or with the victim
// signed into somebody else's GitHub) it ends with the attacker's login resolving
// to the victim's account.
func TestLinkStartRequiresCSRFAndHasNoGETRoute(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	linkCookie := func(w *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if c.Name == oauthLinkStateCookieFor(config.ActorKindGitHub) && c.Value != "" {
				return c
			}
		}
		return nil
	}

	// No token: refused before a state cookie exists, so the flow cannot even be
	// started, let alone completed.
	w := postLinkStart(t, h, sid, "", config.ActorKindGitHub)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 for a link start with no CSRF token", w.Code)
	}
	if c := linkCookie(w); c != nil {
		t.Fatalf("a tokenless link start minted state %q", c.Value)
	}

	// A wrong token is no better than none.
	if w := postLinkStart(t, h, sid, "not-the-token", config.ActorKindGitHub); w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 for a forged CSRF token", w.Code)
	}

	// And the old GET route is gone: it is what the attack navigated to, and no
	// token can be attached to a link somebody else planted.
	for _, p := range []string{"/auth/github/link", "/auth/google/link", "/auth/discord/link"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(sessCookie(sid))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status=%d — a link must not start from a navigation", p, w.Code)
		}
		if c := linkCookie(w); c != nil {
			t.Fatalf("GET %s minted link state %q", p, c.Value)
		}
	}

	// A cross-site POST is refused too, on the header a browser sets itself —
	// belt and braces behind the token.
	form := url.Values{"provider": {config.ActorKindGitHub}, "csrf": {csrfFor(t, h, sid)}}
	req := httptest.NewRequest(http.MethodPost, "/account/link", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(sessCookie(sid))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if c := linkCookie(w); c != nil {
		t.Fatalf("a cross-site link start minted state %q", c.Value)
	}

	// With the token, from our own page, it works — and the whole flow still
	// completes, so the fix did not just close the door on the feature.
	state := startLink(t, h, config.ActorKindGitHub, sid)
	if state == nil {
		t.Fatal("a same-origin link start with a valid token issued no state cookie")
	}
	if msg := redirectErr(t, finishLink(t, h, config.ActorKindGitHub, state, "h-admin", sid)); msg != "" {
		t.Fatalf("link refused: %q", msg)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "admin-1" {
		t.Fatalf("Canonical(github:999)=%q want admin-1", got)
	}
}

// The same refusal with no session at all — a callback replayed from a browser
// that is not signed in has nothing to attach the login to.
func TestLinkRefusedWithNoSession(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	state := startLink(t, h, config.ActorKindGitHub, sid)
	if state == nil {
		t.Fatal("no link state cookie")
	}

	w := finishLink(t, h, config.ActorKindGitHub, state, "h-admin", "")
	if got := f.srv.identity.Canonical("github:999"); got != "github:999" {
		t.Fatalf("github:999 was linked to %q with no session present", got)
	}
	if msg := redirectErr(t, w); !strings.Contains(msg, "no signed-in session") {
		t.Fatalf("err=%q", msg)
	}
	if f.github.Exchanges != 0 {
		t.Fatalf("exchanges=%d — refused only after redeeming the code", f.github.Exchanges)
	}
}

// A LOGIN flow must never attach anything to the browser's current account.
//
// The session binding lives only on the link cookie, so if an ordinary login
// state could reach the link branch, the whole CSRF defence would be bypassable
// by starting a plain login instead.
func TestLoginFlowNeverLinks(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(false, true)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "github:999")
	})
	linkFixture(t, f, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	// An ordinary GitHub login, completed in a browser already signed in as
	// admin-1.
	_, state := startOAuth(t, h, config.ActorKindGitHub, "")
	if state == nil {
		t.Fatal("no login state cookie")
	}
	w := finishLink(t, h, config.ActorKindGitHub, state, "h-admin", sid)
	if sessionCookie(w) == "" {
		t.Fatalf("the login itself failed (loc=%q)", w.Header().Get("Location"))
	}
	if len(f.srv.identity.AliasesOf("admin-1")) != 0 {
		t.Fatal("a plain login attached a second identity to the signed-in account")
	}
}

// An abandoned link flow — the tab someone closed at the provider's consent
// screen — leaves its cookie behind for ten minutes. Choosing the flow by which
// cookie EXISTS rather than which state matches would let it swallow the next
// genuine login for that provider.
func TestAbandonedLinkFlowDoesNotSwallowALogin(t *testing.T) {
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(false, true)(cfg)
		cfg.WebAuth.AdminDiscordIDs = append(cfg.WebAuth.AdminDiscordIDs, "github:999")
	})
	linkFixture(t, f, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	stale := startLink(t, h, config.ActorKindGitHub, sid)
	if stale == nil {
		t.Fatal("no link state cookie")
	}
	_, login := startOAuth(t, h, config.ActorKindGitHub, "")
	if login == nil {
		t.Fatal("no login state cookie")
	}
	loginState, _, _ := strings.Cut(login.Value, "|")
	req := httptest.NewRequest(http.MethodGet,
		authCallbackPath(config.ActorKindGitHub)+"?code=h-admin&state="+url.QueryEscape(loginState), nil)
	req.AddCookie(stale)
	req.AddCookie(login)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if sessionCookie(w) == "" {
		t.Fatalf("a stale link cookie broke a genuine login (err=%q)", redirectErr(t, w))
	}
}

// --- happy path + absorb -----------------------------------------------------

// absorbFixture is a deployment where "github:999" already has a footprint: web
// role, project access, a team, a capability mapping, and a work unit. That is
// the normal case, not an exotic one — it is what happens to anyone who used
// the tool through one login before linking.
func absorbFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f := newAuthFixture(t, func(cfg *config.Config) {
		withProviders(false, true)(cfg)
		cfg.WebAuth.MemberDiscordIDs = append(cfg.WebAuth.MemberDiscordIDs, "github:999")
		pc := cfg.Projects["proj"]
		pc.AllowedUserIDs = append(pc.AllowedUserIDs, "github:999")
		pc.Teams = map[string]config.TeamConfig{
			"support": {Label: "Support", Members: []string{"github:999"}, Capabilities: "investigator"},
		}
		pc.CapabilityByUser = map[string]string{"github:999": "investigator"}
		cfg.Projects["proj"] = pc
	})
	linkFixture(t, f, nil)
	if err := f.srv.sessions.Set("t-owned", sessionstore.Entry{
		SessionID:  "s-owned",
		Project:    "proj",
		OwnerID:    "github:999",
		CoOwnerIDs: []string{"admin-1"},
		WatcherIDs: []string{"github:999"},
		CreatedBy:  "github:999",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return f, sid
}

func TestLinkAbsorbsGrantsAndOwnership(t *testing.T) {
	f, sid := absorbFixture(t)
	w := linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)
	if msg := redirectErr(t, w); msg != "" {
		t.Fatalf("link refused: %q", msg)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "admin-1" {
		t.Fatalf("Canonical(github:999)=%q want admin-1", got)
	}

	saved := savedConfig(t, f)
	if !containsActor(saved.WebAuth.MemberDiscordIDs, "admin-1") ||
		containsActor(saved.WebAuth.MemberDiscordIDs, "github:999") {
		t.Fatalf("web member list=%v", saved.WebAuth.MemberDiscordIDs)
	}
	proj := saved.Projects["proj"]
	if containsActor(proj.AllowedUserIDs, "github:999") || !containsActor(proj.AllowedUserIDs, "admin-1") {
		t.Fatalf("allowlist=%v", proj.AllowedUserIDs)
	}
	if !containsActor(proj.Teams["support"].Members, "admin-1") ||
		containsActor(proj.Teams["support"].Members, "github:999") {
		t.Fatalf("team members=%v", proj.Teams["support"].Members)
	}
	if _, ok := proj.CapabilityByUser["github:999"]; ok {
		t.Fatalf("capabilityByUser kept the alias: %v", proj.CapabilityByUser)
	}
	if proj.CapabilityByUser["admin-1"] != "investigator" {
		t.Fatalf("capabilityByUser=%v", proj.CapabilityByUser)
	}

	e, ok := f.srv.sessions.Get("t-owned")
	if !ok {
		t.Fatal("unit vanished")
	}
	if e.OwnerID != "admin-1" || e.CreatedBy != "admin-1" {
		t.Fatalf("unit actors: owner=%q createdBy=%q", e.OwnerID, e.CreatedBy)
	}
	if len(e.WatcherIDs) != 1 || e.WatcherIDs[0] != "admin-1" {
		t.Fatalf("watchers=%v", e.WatcherIDs)
	}
	// The account was already a co-owner; absorbing must not list it twice, nor
	// leave it as both owner and co-owner.
	if len(e.CoOwnerIDs) != 0 {
		t.Fatalf("coOwners=%v want the account not duplicated onto its own thread", e.CoOwnerIDs)
	}

	// The handle is cached at link time and is what git attribution builds the
	// noreply address from — it can only come from the login the person just
	// proved, so losing it here means no trailer later.
	if login, numericID, ok := f.srv.identity.GitHubFor("admin-1"); !ok || login != "alice" || numericID != "999" {
		t.Fatalf("GitHubFor=(%q,%q,%v) want alice/999", login, numericID, ok)
	}

	ev := lastAudit(t, f, audit.ActionIdentityLink)
	if !ev.OK || ev.Actor != "admin-1" {
		t.Fatalf("audit=%+v", ev)
	}
	if ev.Detail["alias"] != "github:999" {
		t.Fatalf("audit detail must name the linked login: %+v", ev.Detail)
	}
	if ev.Detail["grants"] == nil || ev.Detail["units"] == nil {
		t.Fatalf("audit detail must carry the rewrite counts: %+v", ev.Detail)
	}
}

// Linking again is the recovery path when a rewrite failed halfway, so it has
// to be safe: no duplicated grants, no second alias row.
func TestLinkIsIdempotent(t *testing.T) {
	f, sid := absorbFixture(t)
	if msg := redirectErr(t, linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)); msg != "" {
		t.Fatalf("first link refused: %q", msg)
	}
	if msg := redirectErr(t, linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)); msg != "" {
		t.Fatalf("re-linking the same login refused: %q", msg)
	}
	if got := f.srv.identity.AliasesOf("admin-1"); len(got) != 1 || got[0] != "github:999" {
		t.Fatalf("aliases=%v", got)
	}
	saved := savedConfig(t, f)
	if n := countActor(saved.WebAuth.MemberDiscordIDs, "admin-1"); n != 1 {
		t.Fatalf("member list names the account %d times: %v", n, saved.WebAuth.MemberDiscordIDs)
	}
}

// A session minted before the link carries the alias, which after the link
// names nobody: its grants have moved and it can never be minted again.
func TestLinkRevokesTheAliasSessions(t *testing.T) {
	f, sid := absorbFixture(t)
	aliasSID, _, err := f.srv.LoginAs("github:999", "Alice GH", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.srv.webSessions.Get(aliasSID); !ok {
		t.Fatal("fixture session missing")
	}
	if msg := redirectErr(t, linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)); msg != "" {
		t.Fatalf("link refused: %q", msg)
	}
	if _, _, ok := f.srv.webSessions.Get(aliasSID); ok {
		t.Fatal("the alias's session survived the link and still acts as a hollow identity")
	}
	// The linking session is untouched — it is the account's.
	if _, _, ok := f.srv.webSessions.Get(sid); !ok {
		t.Fatal("the account's own session was revoked")
	}
	ev := lastAudit(t, f, audit.ActionIdentityLink)
	if ev.Detail["sessionsRevoked"] == nil {
		t.Fatalf("audit detail=%+v", ev.Detail)
	}
}

// One login, one account. Moving it silently would move every grant that
// resolves through it, so the owner has to unlink it first.
func TestLinkRefusedWhenLoginBelongsToAnotherAccount(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, map[string]string{"github:999": "someone-else"})
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)
	if msg := redirectErr(t, w); !strings.Contains(msg, "already linked to another account") {
		t.Fatalf("err=%q", msg)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "someone-else" {
		t.Fatalf("the link moved to %q", got)
	}
	if ev := lastAudit(t, f, audit.ActionIdentityLink); ev.OK {
		t.Fatalf("refusal audited as success: %+v", ev)
	}
}

// Attaching an account that has logins of its own is a MERGE, which is out of
// scope — and would silently orphan whichever set lost.
func TestLinkRefusedWhenLoginIsItselfAnAccount(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, true))
	linkFixture(t, f, map[string]string{"google:sub-1": "github:999"})
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)
	if msg := redirectErr(t, w); !strings.Contains(msg, "merging two accounts is not supported") {
		t.Fatalf("err=%q", msg)
	}
	if got := f.srv.identity.Canonical("google:sub-1"); got != "github:999" {
		t.Fatalf("the other account's links changed: %q", got)
	}
}

// --- unlink ------------------------------------------------------------------

func postUnlink(t *testing.T, f *authFixture, sid, csrf, alias string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"alias": {alias}}
	if csrf != "" {
		form.Set("csrf", csrf)
	}
	req := httptest.NewRequest(http.MethodPost, "/account/unlink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sid != "" {
		req.AddCookie(sessCookie(sid))
	}
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

// Unlinking the only login that can reach the account locks the person out of
// their own grants, with nobody but an admin able to undo it.
func TestUnlinkRefusedWhenItWouldLockTheAccountOut(t *testing.T) {
	// Google-only deployment: the canonical is a legacy Discord snowflake that
	// nothing can authenticate any more, so the Google alias is the only door.
	f := newAuthFixture(t, func(cfg *config.Config) {
		cfg.DiscordClientID = ""
		cfg.DiscordClientSecret = ""
		cfg.DiscordToken = ""
		cfg.WebAuth.Providers = &config.WebAuthProviders{
			Google: &config.OAuthProviderConfig{ClientID: "google-client-id", ClientSecret: "google-secret"},
		}
	})
	linkFixture(t, f, map[string]string{"google:sub-1": "admin-1"})
	sid, csrf, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postUnlink(t, f, sid, csrf, "google:sub-1")
	if msg := redirectErr(t, w); !strings.Contains(msg, "only login that can reach this account") {
		t.Fatalf("err=%q", msg)
	}
	if got := f.srv.identity.Canonical("google:sub-1"); got != "admin-1" {
		t.Fatalf("the link was removed anyway: %q", got)
	}
	if ev := lastAudit(t, f, audit.ActionIdentityUnlink); ev.OK {
		t.Fatalf("refusal audited as success: %+v", ev)
	}
}

// Unlinking does NOT move absorbed grants back: they were rewritten onto the
// account and belong to it. The UI says so before anyone clicks.
func TestUnlinkKeepsAbsorbedGrants(t *testing.T) {
	f, sid := absorbFixture(t)
	if msg := redirectErr(t, linkVia(t, f, config.ActorKindGitHub, "h-admin", sid)); msg != "" {
		t.Fatalf("link refused: %q", msg)
	}
	sess, _, ok := f.srv.webSessions.Get(sid)
	if !ok {
		t.Fatal("session gone")
	}
	w := postUnlink(t, f, sid, sess.CSRF, "github:999")
	if msg := redirectErr(t, w); msg != "" {
		t.Fatalf("unlink refused: %q", msg)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "github:999" {
		t.Fatalf("still linked: %q", got)
	}
	saved := savedConfig(t, f)
	if !containsActor(saved.WebAuth.MemberDiscordIDs, "admin-1") {
		t.Fatalf("absorbed grant was rolled back: %v", saved.WebAuth.MemberDiscordIDs)
	}
	if ev := lastAudit(t, f, audit.ActionIdentityUnlink); !ev.OK || ev.Detail["alias"] != "github:999" {
		t.Fatalf("audit=%+v", ev)
	}
}

// Somebody else's alias is not yours to detach, and a login that is not linked
// at all answers identically — the difference would make this a probe.
func TestUnlinkRefusesForeignAlias(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, map[string]string{"github:999": "someone-else"})
	sid, csrf, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	foreign := redirectErr(t, postUnlink(t, f, sid, csrf, "github:999"))
	missing := redirectErr(t, postUnlink(t, f, sid, csrf, "github:4242"))
	if foreign != missing {
		t.Fatalf("a foreign alias (%q) and an unknown one (%q) must be indistinguishable", foreign, missing)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "someone-else" {
		t.Fatalf("another account's link was removed: %q", got)
	}
}

func TestUnlinkRequiresCSRF(t *testing.T) {
	f := newAuthFixture(t, withProviders(false, true))
	linkFixture(t, f, map[string]string{"github:999": "admin-1"})
	sid, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	w := postUnlink(t, f, sid, "", "github:999")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	if got := f.srv.identity.Canonical("github:999"); got != "admin-1" {
		t.Fatalf("unlinked without a CSRF token: %q", got)
	}
}

// --- the page ----------------------------------------------------------------

// A viewer's logins are as much theirs as an admin's, so the page and its
// mutations are gated on being signed in and nothing else.
func TestAccountPageRendersIdentitiesAndLinkOptions(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, true))
	linkFixture(t, f, map[string]string{"github:999": "viewer-1"})
	sid, _, err := f.srv.LoginAs("viewer-1", "Vic", config.WebRoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(sessCookie(sid))
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-account"`,
		`id="account-identities"`,
		`id="account-link"`,
		"viewer-1",
		"github:999",
		"canonical",
		`action="/account/unlink"`,
		`name="csrf"`,
		`action="/account/link"`,
		// Only Google is configured-and-unlinked: Discord is the canonical's own
		// provider and GitHub is already an alias.
		`value="google"`,
		`id="account-link-google"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("account page missing %q", want)
		}
	}
	for _, unwanted := range []string{`id="account-link-github"`, `id="account-link-discord"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("account page offers %q for a provider already on the account", unwanted)
		}
	}
	// No capability gate: a viewer may start a link flow for their own logins.
	// (Gating it would leave the least-privileged person permanently split
	// across two identities with no way to ask for it to be fixed.)
	w = postLinkStart(t, f.srv.Handler(), sid, csrfFor(t, f.srv.Handler(), sid), config.ActorKindGoogle)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "accounts.google.com") {
		t.Fatalf("viewer could not start a link: status=%d loc=%q", w.Code, w.Header().Get("Location"))
	}

	// Signed out, the same route is closed.
	w = postLinkStart(t, f.srv.Handler(), "", "", config.ActorKindGoogle)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anonymous link start: status=%d loc=%q", w.Code, loc)
	}
}

// The account menu is the way in; without a link there the page is unreachable
// except by typing the URL.
func TestAccountMenuLinksToAccountPage(t *testing.T) {
	srv, _, _ := authOnServer(t)
	sid, _, err := srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessCookie(sid))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	start := strings.Index(body, `<div class="side-user-pop" id="user-menu">`)
	if start < 0 {
		t.Fatal("account pop missing")
	}
	end := strings.Index(body[start:], "</div>")
	if !strings.Contains(body[start:start+end], `href="/account"`) {
		t.Fatal("the account menu must link to /account")
	}
}

// With web auth off there is no account at all. The page says so rather than
// rendering buttons that would refuse, and the mutations are closed.
func TestAccountPageWithAuthOff(t *testing.T) {
	srv, _, _ := testServer(t)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/account", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-account"`) || !strings.Contains(body, `id="account-anonymous"`) {
		t.Fatalf("auth-off account page: %s", body)
	}
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/account/unlink", strings.NewReader("alias=github:1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unlink with auth off: status=%d", w.Code)
	}
}

// The denial a stranger sees has to name the way out that does not need an
// admin: sign in with the login that already has access and attach this one.
func TestLoginDenialPointsAtLinking(t *testing.T) {
	f := newAuthFixture(t, withProviders(true, false))
	linkFixture(t, f, nil)
	w := loginVia(t, f.srv.Handler(), config.ActorKindGoogle, "g-new", "")
	msg := redirectErr(t, w)
	if !strings.Contains(msg, "google:sub-x") {
		t.Fatalf("err=%q", msg)
	}
	if !strings.Contains(msg, "/account") {
		t.Fatalf("denial does not mention linking: %q", msg)
	}
}

func containsActor(ids []string, want string) bool { return countActor(ids, want) > 0 }

func countActor(ids []string, want string) int {
	n := 0
	for _, id := range ids {
		if config.SameActor(id, want) {
			n++
		}
	}
	return n
}
