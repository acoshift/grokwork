package web

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

type ctxKey int

const sessionCtxKey ctxKey = 1

// LoginAs creates a web session for tests (and returns cookies to attach).
// When auth is disabled this is a no-op that returns empty values.
func (s *Server) LoginAs(discordUserID, displayName string, role config.WebRole) (sessionID, csrf string, err error) {
	if s.webSessions == nil {
		return "", "", nil
	}
	sess, err := s.webSessions.Create(discordUserID, displayName, "", role)
	if err != nil {
		return "", "", err
	}
	if s.webUsers != nil {
		_ = s.webUsers.Upsert(discordUserID, displayName, "")
	}
	return sess.ID, sess.CSRF, nil
}

// cookieSecure is true when the public base URL is https (avoid breaking http:// Tailscale binds).
func (s *Server) cookieSecure() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(s.cfg.WebPublicBaseURLValue()), "https://")
}

// SetSessionCookie writes the session cookie onto a response (tests / handlers).
func (s *Server) SetSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// SetSessionCookie is a package-level helper for tests that lack a Server pointer.
func SetSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func (s *Server) sessionFromRequest(r *http.Request) *Session {
	sess, _ := s.sessionFromRequestRenew(r)
	return sess
}

// sessionFromRequestRenew is like sessionFromRequest but reports whether the
// server-side session was sliding-renewed (caller should re-set the cookie).
func (s *Server) sessionFromRequestRenew(r *http.Request) (*Session, bool) {
	if s.webSessions == nil {
		return nil, false
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	sess, renewed, ok := s.webSessions.Get(c.Value)
	if !ok {
		return nil, false
	}
	return sess, renewed
}

func sessionFromContext(ctx context.Context) *Session {
	v, _ := ctx.Value(sessionCtxKey).(*Session)
	return v
}

func withSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey, sess)
}

func (s *Server) checkCSRF(r *http.Request, sess *Session) bool {
	if sess == nil || sess.CSRF == "" {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		token = r.FormValue("csrf")
	}
	if token == "" {
		_ = r.ParseForm()
		token = r.PostFormValue("csrf")
	}
	if token == "" || sess.CSRF == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) == 1
}

// isNonNavigableWebPath is true for endpoints that are not full pages after login
// (SSE stream, live-region partials, OAuth).
func isNonNavigableWebPath(p string) bool {
	return strings.HasPrefix(p, "/partials/") || p == "/events" || strings.HasPrefix(p, "/auth/")
}

// loginNextFromRequest picks a post-login path. Boosted navigations use the
// request path (the page the user tried to open). Live-region partials use the
// fragment URL as the request path, so recover the browser page via HX-Current-URL.
func loginNextFromRequest(r *http.Request) string {
	next := r.URL.RequestURI()
	if isNonNavigableWebPath(next) && r.Header.Get("HX-Request") == "true" {
		if cur := strings.TrimSpace(r.Header.Get("HX-Current-URL")); cur != "" {
			if u, err := url.Parse(cur); err == nil {
				next = u.RequestURI()
			}
		}
	}
	next = safeLocalNext(next)
	if isNonNavigableWebPath(next) {
		return "/"
	}
	return next
}

// requireAuth redirects unauthenticated users to /login when web auth is enabled.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.WebAuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		sess, renewed := s.sessionFromRequestRenew(r)
		if sess == nil {
			loginURL := "/login?next=" + url.QueryEscape(loginNextFromRequest(r))
			// htmx follows HTTP 302 and swaps the final body into the request
			// target (each live-region / #live-root), so every component shows
			// the login page. Force a full document navigation instead.
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", loginURL)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, loginURL, http.StatusFound)
			return
		}
		// Keep the browser cookie aligned after a sliding renew so MaxAge does
		// not lag the server-side ExpiresAt.
		if renewed {
			s.SetSessionCookie(w, sess.ID)
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

// requireAdmin enforces admin role + CSRF for mutating POSTs when auth is enabled.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.WebAuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil {
			sess = s.sessionFromRequest(r)
		}
		if sess == nil || !config.RoleAtLeast(sess.Role, config.WebRoleAdmin) {
			http.Error(w, "forbidden: admin required", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if !s.checkCSRF(r, sess) {
				http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	}))
}

// safeLocalNext returns a same-origin relative path for post-login redirects.
// Rejects protocol-relative (//host) and backslash tricks that browsers treat as external.
func safeLocalNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") {
		return "/"
	}
	// //evil.example and /\evil are open redirects in browsers.
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

func (s *Server) loginPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Log in"
	d.IsLogin = true
	d.SSEPath = "" // login is public; do not open authenticated SSE
	rawNext := strings.TrimSpace(ctx.FormValue("next"))
	d.LoginNext = safeLocalNext(rawNext)
	if d.LoginNext == "/" && rawNext != "" && rawNext != "/" {
		// Drop unsafe next from the login form/link entirely.
		d.LoginNext = ""
	}
	d.Error = strings.TrimSpace(ctx.FormValue("err"))
	d.LoginProviders = s.loginProviders()
	if s.cfg.WebAuthEnabled() {
		if sess := s.sessionFromRequest(ctx.Request); sess != nil {
			return ctx.Redirect(safeLocalNext(rawNext))
		}
	} else {
		// Auth off — no login needed.
		return ctx.Redirect("/")
	}
	return s.viewPage(ctx, "login", d)
}

// oauthStart begins a login for the provider named in the path. One handler
// serves every provider: see oauthProvider for why the per-provider surface is
// deliberately four small pure functions.
func (s *Server) oauthStart(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	key := ctx.PathValue("provider")
	p, known := s.provider(key)
	if !known {
		return ctx.Status(http.StatusNotFound).Error("unknown login provider")
	}
	// Fail closed before anything is minted: no state cookie is written for a
	// provider whose credentials are missing.
	if !s.providerConfigured(key) {
		return ctx.RedirectTo("login", map[string]string{"err": p.Label() + " login is not configured"})
	}
	clientID, _ := s.cfg.OAuthProviderCreds(key)
	redirectURI := s.oauthRedirectURIFor(key)
	state, err := randomToken(16)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("state: " + err.Error())
	}
	next := safeLocalNext(ctx.FormValue("next"))
	// Store state (+ optional next) in short-lived cookie.
	val := state
	if next != "/" {
		val = state + "|" + next
	}
	http.SetCookie(ctx.ResponseWriter(), &http.Cookie{
		Name:     oauthStateCookieFor(key),
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(),
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	authURL := p.AuthorizeURL(clientID, redirectURI, state)
	// Boosted htmx would follow a 302 to the provider with HX-Request, which
	// their CORS rejects ("HX-Request is not allowed by
	// Access-Control-Allow-Headers"). Force a client-side full navigation
	// instead. Login links also use hx-boost=false.
	if ctx.Request.Header.Get("HX-Request") == "true" {
		ctx.ResponseWriter().Header().Set("HX-Redirect", authURL)
		ctx.ResponseWriter().WriteHeader(http.StatusNoContent)
		return nil
	}
	return ctx.Redirect(authURL)
}

// oauthLinkStart begins attaching another login to the signed-in account.
//
// It is oauthStart with two differences, and both are the whole security model:
// the state lands in the LINK cookie (so it can never complete a login), and
// the cookie carries the id of the web session that started it (so the callback
// can refuse to attach an identity to a session that did not ask for it).
//
// A GET is safe here even though it has no CSRF token: forging this request
// only makes the victim's browser start a flow that will attach whatever the
// VICTIM authenticates as, to the victim's own account. The dangerous
// direction — the attacker's identity onto the victim's account — is what the
// session binding refuses.
func (s *Server) oauthLinkStart(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	key := ctx.PathValue("provider")
	p, known := s.provider(key)
	if !known {
		return ctx.Status(http.StatusNotFound).Error("unknown login provider")
	}
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return ctx.RedirectTo("login", map[string]string{"err": "log in before linking another login"})
	}
	// Same gate as the login route: a hidden button is not a gate, and no state
	// cookie is written for a provider whose credentials are missing.
	if !s.providerConfigured(key) {
		return s.accountRedirect(ctx, "", p.Label()+" login is not configured")
	}
	if s.identity == nil {
		return s.accountRedirect(ctx, "", "identity linking is not available")
	}
	// Shares the session-start budget: a link is a provider round trip started
	// by a click, and the same actor hammering it is the same abuse.
	if !s.startLimiter().Allow(sess.DiscordUserID) {
		err := fmt.Errorf("rate limit exceeded: try again in a minute")
		s.auditIdentity(ctx, audit.ActionIdentityLink, err, map[string]any{"provider": key})
		return s.accountRedirect(ctx, "", err.Error())
	}
	state, err := randomToken(16)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("state: " + err.Error())
	}
	http.SetCookie(ctx.ResponseWriter(), &http.Cookie{
		Name:     oauthLinkStateCookieFor(key),
		Value:    state + "|" + sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(),
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	clientID, _ := s.cfg.OAuthProviderCreds(key)
	authURL := p.AuthorizeURL(clientID, s.oauthRedirectURIFor(key), state)
	// Same reason as oauthStart: boosted htmx would call the provider with an
	// HX-Request header their CORS rejects.
	if ctx.Request.Header.Get("HX-Request") == "true" {
		ctx.ResponseWriter().Header().Set("HX-Redirect", authURL)
		ctx.ResponseWriter().WriteHeader(http.StatusNoContent)
		return nil
	}
	return ctx.Redirect(authURL)
}

// oauthFlow is what the state cookie says a callback is completing.
type oauthFlow struct {
	// link is true for a link flow; false is an ordinary login.
	link bool
	// sessionID is the web session that started a link flow.
	sessionID string
	// next is a login flow's post-login path (always safeLocalNext'd).
	next string
}

// takeOAuthState consumes the state cookies for this provider and reports which
// flow the returned state belongs to. The string result is a user-facing
// refusal, empty on success.
//
// Both cookies are cleared whether or not they matched: a state is single-use,
// and leaving the loser behind would let it be replayed for its remaining TTL.
//
// The flow is chosen by which cookie's state MATCHES, not by which cookie
// exists. Preferring the link cookie on presence alone would let an abandoned
// link flow — the tab someone closed at the provider's consent screen —
// swallow a genuine login callback for the next ten minutes.
func (s *Server) takeOAuthState(ctx *hime.Context, key, state string) (oauthFlow, string) {
	read := func(name string) string {
		c, err := ctx.Cookie(name)
		if err != nil || c.Value == "" {
			return ""
		}
		http.SetCookie(ctx.ResponseWriter(), &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cookieSecure(),
			Expires: time.Unix(0, 0),
		})
		return c.Value
	}
	linkVal := read(oauthLinkStateCookieFor(key))
	loginVal := read(oauthStateCookieFor(key))
	if linkVal == "" && loginVal == "" {
		return oauthFlow{}, "missing OAuth state cookie"
	}
	if linkVal != "" {
		stored, sessionID, _ := strings.Cut(linkVal, "|")
		if stored == state {
			return oauthFlow{link: true, sessionID: sessionID}, ""
		}
	}
	if loginVal != "" {
		stored, next, found := strings.Cut(loginVal, "|")
		if stored == state {
			f := oauthFlow{next: "/"}
			if found {
				f.next = safeLocalNext(next)
			}
			return f, ""
		}
	}
	return oauthFlow{}, "invalid OAuth state"
}

// oauthCallback completes a login. Nothing from the browser is trusted but the
// opaque code and the state this server issued — no user id, email or role is
// ever read from a query parameter. Every refusal below happens before any
// network call, so an unconfigured or unknown provider cannot exchange.
func (s *Server) oauthCallback(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	key := ctx.PathValue("provider")
	p, known := s.provider(key)
	if !known {
		return ctx.Status(http.StatusNotFound).Error("unknown login provider")
	}
	if !s.providerConfigured(key) {
		return ctx.RedirectTo("login", map[string]string{"err": p.Label() + " login is not configured"})
	}
	q := ctx.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		return ctx.RedirectTo("login", map[string]string{"err": p.Label() + " denied login: " + errParam})
	}
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		return ctx.RedirectTo("login", map[string]string{"err": "missing OAuth code or state"})
	}
	flow, stateErr := s.takeOAuthState(ctx, key, state)
	if stateErr != "" {
		return ctx.RedirectTo("login", map[string]string{"err": stateErr})
	}
	// A link is authorized by the session that STARTED it, and that is checked
	// before the code is redeemed: an attacker who lures a signed-in victim into
	// completing their link callback must not even get their code spent, let
	// alone their login attached. See verifyLinkSession.
	var linkSession *Session
	if flow.link {
		var err error
		linkSession, err = s.verifyLinkSession(ctx, flow)
		if err != nil {
			s.auditIdentity(ctx, audit.ActionIdentityLink, err, map[string]any{"provider": key})
			return s.accountRedirect(ctx, "", err.Error())
		}
	}

	clientID, secret := s.cfg.OAuthProviderCreds(key)
	redirectURI := s.oauthRedirectURIFor(key)
	token, err := p.Exchange(ctx.Context(), code, redirectURI, clientID, secret)
	if err != nil {
		// A link failure belongs on /account: the person is signed in, and
		// bouncing them to /login would drop the message (a live session there
		// redirects straight home).
		if flow.link {
			lerr := fmt.Errorf("token exchange failed")
			s.auditIdentity(ctx, audit.ActionIdentityLink, lerr, map[string]any{"provider": key})
			return s.accountRedirect(ctx, "", lerr.Error())
		}
		s.auditLogin(audit.ActorAnonymous, "", "", false, "token exchange failed")
		return ctx.RedirectTo("login", map[string]string{"err": "token exchange failed"})
	}
	id, err := p.Identity(ctx.Context(), token)
	if err != nil || strings.TrimSpace(id.Subject) == "" {
		if flow.link {
			lerr := fmt.Errorf("failed to load %s profile", p.Label())
			s.auditIdentity(ctx, audit.ActionIdentityLink, lerr, map[string]any{"provider": key})
			return s.accountRedirect(ctx, "", lerr.Error())
		}
		s.auditLogin(audit.ActorAnonymous, "", "", false, "failed to load "+p.Label()+" profile")
		return ctx.RedirectTo("login", map[string]string{"err": "failed to load " + p.Label() + " profile"})
	}
	if flow.link {
		return s.finishOAuthLink(ctx, key, linkSession, id)
	}
	// Canonical-at-mint: this is the only place a web actor id is born, so it is
	// the only place that has to know about linked logins. Everything downstream —
	// the session cookie, role resolution, the durable profile key, ownership,
	// spend, the per-user run cap — sees the ACCOUNT and keeps comparing ids the
	// way it always did. See internal/identity for why the alternative (teaching
	// every comparison about aliases) was rejected.
	loginActor := actorIDFor(key, id.Subject)
	// A GitHub login is mutable and the numeric id is not, so the cached handle
	// is re-proven on every sign-in, not only at link time: attribution writes
	// "@login" into public git history, and a renamed account would otherwise
	// keep being credited under a name that now belongs to someone else. Only a
	// login that is ALREADY an alias is touched — RefreshHandle never creates a
	// binding — so this is a no-op for everyone who has not linked. Best effort:
	// a stale handle must not cost anybody their login.
	if err := s.identity.RefreshHandle(loginActor, id.Handle); err != nil {
		log.Printf("warn: identity: refresh handle for %s: %v", loginActor, err)
	}
	actor := s.identity.Canonical(loginActor)
	role, ok := s.cfg.ResolveWebRoleForConfig(actor)
	if !ok {
		s.auditLogin(actor, loginActor, "", false, "not authorized")
		// A brand-new Google/GitHub account is a member of nothing, which is the
		// correct fail-closed outcome — but the person cannot guess the exact
		// allowlist string, so name it. Discord's message is left byte-identical
		// (its actor id is the snowflake they already know).
		// A Discord login that resolved to some other account is no longer "the
		// snowflake they already know" either, so it gets the id too.
		msg := "not authorized for this Grok Work instance"
		if key != config.ActorKindDiscord || actor != loginActor {
			msg += " (" + actor + ")"
		}
		// The other way in is the one nobody guesses: a person who already has
		// access under a different login does not need an admin at all, they need
		// to sign in with that one and attach this login to it. Say so here, where
		// they are standing, rather than in documentation they are not reading.
		if actor == loginActor && s.linkingAvailable() {
			msg += " — if you already have access with another login, sign in with that one and link this one at /account"
		}
		return ctx.RedirectTo("login", map[string]string{"err": msg})
	}
	name := id.Name
	if strings.TrimSpace(name) == "" {
		name = actor
	}
	avatar := id.AvatarURL
	sess, err := s.webSessions.Create(actor, name, avatar, role)
	if err != nil {
		s.auditLogin(actor, loginActor, string(role), false, "session create failed")
		return ctx.Status(http.StatusInternalServerError).Error("session: " + err.Error())
	}
	// Durable profile (name + avatar URL); not cleared on logout. Keyed by the
	// account, so the name and avatar are whichever login signed in most recently
	// — one person, one profile.
	if s.webUsers != nil {
		_ = s.webUsers.Upsert(actor, name, avatar)
	}
	s.auditLogin(actor, loginActor, string(role), true, "")
	s.SetSessionCookie(ctx.ResponseWriter(), sess.ID)
	return ctx.Redirect(flow.next)
}

// auditLogin records one login attempt.
//
// Both ids are kept. Actor is the account the session will carry, which is what
// every other audit row and every grant is written against; loginActor is the
// login actually used to get in. Recording only the account would make "which of
// their logins was this" unanswerable — and that question is the entire point of
// an audit trail once one person can arrive three ways. It is written only when
// the two differ, so an unlinked login (the overwhelming majority) keeps the row
// it always had.
func (s *Server) auditLogin(actor, loginActor, role string, ok bool, errMsg string) {
	if s == nil || s.audit == nil {
		return
	}
	action := audit.ActionLoginOK
	if !ok {
		action = audit.ActionLoginFail
	}
	ev := audit.Event{Action: action, Actor: actor, Role: role, OK: ok}
	if loginActor != "" && loginActor != actor {
		ev.Detail = map[string]any{"loginActor": loginActor}
	}
	if errMsg != "" {
		ev.Error = errMsg
	}
	_ = s.audit.Append(ev)
}

func (s *Server) logout(ctx *hime.Context) error {
	if c, err := ctx.Cookie(sessionCookieName); err == nil && c.Value != "" && s.webSessions != nil {
		_ = s.webSessions.Delete(c.Value)
	}
	clearSessionCookie(ctx.ResponseWriter(), s.cookieSecure())
	if s.cfg.WebAuthEnabled() {
		return ctx.RedirectTo("login")
	}
	return ctx.Redirect("/")
}
