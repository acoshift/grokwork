package web

import (
	"context"
	"crypto/subtle"
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
	if s.webSessions == nil {
		return nil
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, ok := s.webSessions.Get(c.Value)
	if !ok {
		return nil
	}
	return sess
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
		sess := s.sessionFromRequest(r)
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

func (s *Server) oauthDiscordStart(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	clientID := s.cfg.EffectiveClientID()
	redirectURI := s.oauthRedirectURI()
	if clientID == "" || redirectURI == "" {
		return ctx.RedirectTo("login", map[string]string{"err": "OAuth is not fully configured (client id / public base URL)"})
	}
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
		Name:     oauthStateCookie,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(),
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	authURL := discordAuthorizeURL(clientID, redirectURI, state)
	// Boosted htmx would follow a 302 to Discord with HX-Request, which Discord
	// CORS rejects ("HX-Request is not allowed by Access-Control-Allow-Headers").
	// Force a client-side full navigation instead. Login link also uses hx-boost=false.
	if ctx.Request.Header.Get("HX-Request") == "true" {
		ctx.ResponseWriter().Header().Set("HX-Redirect", authURL)
		ctx.ResponseWriter().WriteHeader(http.StatusNoContent)
		return nil
	}
	return ctx.Redirect(authURL)
}

func (s *Server) oauthDiscordCallback(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	q := ctx.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		return ctx.RedirectTo("login", map[string]string{"err": "Discord denied login: " + errParam})
	}
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		return ctx.RedirectTo("login", map[string]string{"err": "missing OAuth code or state"})
	}
	c, err := ctx.Cookie(oauthStateCookie)
	if err != nil || c.Value == "" {
		return ctx.RedirectTo("login", map[string]string{"err": "missing OAuth state cookie"})
	}
	// Clear state cookie.
	http.SetCookie(ctx.ResponseWriter(), &http.Cookie{
		Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cookieSecure(),
		Expires: time.Unix(0, 0),
	})
	stored := c.Value
	storedState := stored
	next := "/"
	if i := strings.IndexByte(stored, '|'); i >= 0 {
		storedState = stored[:i]
		next = safeLocalNext(stored[i+1:])
	}
	if storedState != state {
		return ctx.RedirectTo("login", map[string]string{"err": "invalid OAuth state"})
	}

	redirectURI := s.oauthRedirectURI()
	clientID := s.cfg.EffectiveClientID()
	secret := s.cfg.DiscordClientSecretValue()
	oauth := s.oauth
	if oauth == nil {
		oauth = &HTTPDiscordOAuth{}
	}
	token, err := oauth.ExchangeCode(ctx.Context(), code, redirectURI, clientID, secret)
	if err != nil {
		s.auditLogin(audit.ActorAnonymous, "", false, "token exchange failed")
		return ctx.RedirectTo("login", map[string]string{"err": "token exchange failed"})
	}
	user, err := oauth.FetchUser(ctx.Context(), token)
	if err != nil {
		s.auditLogin(audit.ActorAnonymous, "", false, "failed to load Discord profile")
		return ctx.RedirectTo("login", map[string]string{"err": "failed to load Discord profile"})
	}
	role, ok := s.cfg.ResolveWebRoleForConfig(user.ID)
	if !ok {
		s.auditLogin(user.ID, "", false, "not authorized")
		return ctx.RedirectTo("login", map[string]string{"err": "not authorized for this Grok Work instance"})
	}
	sess, err := s.webSessions.Create(user.ID, user.DisplayName(), user.AvatarURL(), role)
	if err != nil {
		s.auditLogin(user.ID, string(role), false, "session create failed")
		return ctx.Status(http.StatusInternalServerError).Error("session: " + err.Error())
	}
	s.auditLogin(user.ID, string(role), true, "")
	s.SetSessionCookie(ctx.ResponseWriter(), sess.ID)
	return ctx.Redirect(next)
}

func (s *Server) auditLogin(actor, role string, ok bool, errMsg string) {
	if s == nil || s.audit == nil {
		return
	}
	action := audit.ActionLoginOK
	if !ok {
		action = audit.ActionLoginFail
	}
	ev := audit.Event{Action: action, Actor: actor, Role: role, OK: ok}
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

func (s *Server) oauthRedirectURI() string {
	base := s.cfg.WebPublicBaseURLValue()
	if base == "" {
		return ""
	}
	return base + "/auth/discord/callback"
}
