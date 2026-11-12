package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/logintoken"
)

// loginTokenRow is one management-page row. It has no secret and no hash.
type loginTokenRow struct {
	ID        string
	Created   string
	Expiry    string
	Status    string
	CanRevoke bool
}

// requireLoginTokenManager gates the login-token page.
//
// A session established by a token cannot list, create, or revoke for its
// whole life. Writes also need the account CSRF token. With web auth off the
// page is inert (GET explains that) and mutations are refused, the same way
// account management is.
func (s *Server) requireLoginTokenManager(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.WebAuthEnabled() {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "forbidden: web auth is disabled, so there are no login tokens to manage", http.StatusForbidden)
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil {
			sess = s.sessionFromRequest(r)
		}
		if sess == nil {
			http.Error(w, "forbidden: log in first", http.StatusForbidden)
			return
		}
		if sess.TokenLogin {
			if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
				action := audit.ActionLoginTokenCreate
				if strings.HasSuffix(r.URL.Path, "/revoke") {
					action = audit.ActionLoginTokenRevoke
				}
				s.auditSession(sess, action, errors.New("token session cannot manage login tokens"), nil)
			}
			http.Error(w, "forbidden: a token session cannot manage login tokens", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !s.checkCSRF(r, sess) {
				http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	}))
}

// loginWithToken is POST /login. The posted secret is not written to the
// audit row, the redirect, or an error string.
func (s *Server) loginWithToken(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		return ctx.Redirect("/")
	}
	next := safeLocalNext(ctx.PostFormValue("next"))
	fail := func(msg string) error {
		q := map[string]string{"err": msg}
		if next != "/" {
			q["next"] = next
		}
		return ctx.RedirectTo("login", q)
	}
	raw := strings.TrimSpace(ctx.PostFormValue("token"))
	if raw == "" {
		s.auditLoginToken(audit.ActorAnonymous, "", false, "", "missing token")
		return fail("Enter a login token")
	}
	if s.loginTokens == nil {
		s.auditLoginToken(audit.ActorAnonymous, "", false, "", "invalid token")
		return fail("Invalid or expired token")
	}
	rec, err := s.loginTokens.Authenticate(raw)
	if err != nil {
		s.auditLoginToken(audit.ActorAnonymous, "", false, logintoken.AuthTokenID(err), loginTokenFailReason(err))
		return fail("Invalid or expired token")
	}
	actor := rec.OwnerID
	if s.identity != nil {
		actor = s.identity.Canonical(rec.OwnerID)
	}
	role, ok := s.cfg.ResolveWebRoleForConfig(actor)
	if !ok {
		s.auditLoginToken(actor, "", false, rec.ID, "not authorized")
		return fail("not authorized for this Grok Work instance (" + actor + ")")
	}
	name, avatar := s.loginTokenProfile(actor)
	sess, err := s.webSessions.CreateTokenLogin(actor, name, avatar, role)
	if err != nil {
		s.auditLoginToken(actor, string(role), false, rec.ID, "session create failed")
		return ctx.Status(http.StatusInternalServerError).Error("session: " + err.Error())
	}
	s.auditLoginToken(actor, string(role), true, rec.ID, "")
	s.SetSessionCookie(ctx.ResponseWriter(), sess.ID)
	return ctx.Redirect(next)
}

func loginTokenFailReason(err error) string {
	switch {
	case errors.Is(err, logintoken.ErrRevoked):
		return "revoked"
	case errors.Is(err, logintoken.ErrExpired):
		return "expired"
	default:
		return "invalid token"
	}
}

// loginTokenProfile uses the durable account profile when one exists.
// A token login has no provider payload, so an unknown account is named by its id.
func (s *Server) loginTokenProfile(actor string) (name, avatar string) {
	name = actor
	if s.webUsers == nil {
		return name, ""
	}
	p, ok := s.webUsers.Get(actor)
	if !ok {
		return name, ""
	}
	if strings.TrimSpace(p.DisplayName) != "" {
		name = p.DisplayName
	}
	return name, p.AvatarURL
}

// auditSession records a mutation for a session the middleware already resolved.
// The handlers use auditAction; this exists for refusals that never reach one.
func (s *Server) auditSession(sess *Session, action string, err error, detail map[string]any) {
	if s == nil || s.audit == nil {
		return
	}
	actor := audit.ActorAnonymous
	role := ""
	if sess != nil && strings.TrimSpace(sess.DiscordUserID) != "" {
		actor = sess.DiscordUserID
		role = string(sess.Role)
	}
	ev := audit.Event{Action: action, Actor: actor, Role: role, Detail: detail, OK: err == nil}
	if err != nil {
		ev.Error = err.Error()
	}
	_ = s.audit.Append(ev)
}

func (s *Server) auditLoginToken(actor, role string, ok bool, tokenID, errMsg string) {
	if s == nil || s.audit == nil {
		return
	}
	action := audit.ActionLoginTokenOK
	if !ok {
		action = audit.ActionLoginTokenFail
	}
	ev := audit.Event{Action: action, Actor: actor, Role: role, OK: ok}
	if id := safeTokenID(tokenID); id != "" {
		ev.Detail = map[string]any{"tokenId": id}
	}
	if errMsg != "" {
		ev.Error = errMsg
	}
	_ = s.audit.Append(ev)
}

// safeTokenID returns id when it is a public login-token id.
// Anything else — including a pasted plaintext token — is dropped so it cannot
// reach an audit row.
func safeTokenID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) != 16 {
		return ""
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return id
}

func (s *Server) loginTokensPage(ctx *hime.Context) error {
	d := s.loginTokensPageData(ctx)
	if !s.cfg.WebAuthEnabled() {
		d.LoginTokensUnavailable = true
		return s.viewPage(ctx, "login_tokens", d)
	}
	sess := sessionFromContext(ctx.Context())
	d.LoginTokenRows = s.loginTokenRows(sess.DiscordUserID)
	d.Error = strings.TrimSpace(ctx.FormValue("err"))
	return s.viewPage(ctx, "login_tokens", d)
}

func (s *Server) loginTokensPageData(ctx *hime.Context) pageData {
	d := s.basePage(ctx)
	d.Title = "Login tokens"
	d.IsAccount = true
	return d
}

func (s *Server) loginTokenRows(ownerID string) []loginTokenRow {
	if s.loginTokens == nil {
		return nil
	}
	now := s.loginTokens.Now()
	recs := s.loginTokens.ListByOwner(ownerID)
	rows := make([]loginTokenRow, 0, len(recs))
	for _, rec := range recs {
		row := loginTokenRow{
			ID:      rec.ID,
			Created: rec.CreatedAt.In(time.Local).Format("2006-01-02 15:04"),
			Expiry:  formatLoginTokenExpiry(rec.ExpiresAt, time.Local),
			Status:  "usable",
		}
		switch {
		case !rec.RevokedAt.IsZero():
			row.Status = "revoked"
		case !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt):
			row.Status = "expired"
			row.CanRevoke = true
		default:
			row.CanRevoke = true
		}
		rows = append(rows, row)
	}
	return rows
}

// formatLoginTokenExpiry prints a date-only expiry as the calendar date it
// covers. The store keeps the exclusive end (local midnight of the next day).
func formatLoginTokenExpiry(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "none"
	}
	if loc == nil {
		loc = time.Local
	}
	local := t.In(loc)
	if local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 && local.Nanosecond() == 0 {
		return local.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return local.Format("2006-01-02 15:04")
}

func (s *Server) postLoginTokenCreate(ctx *hime.Context) error {
	sess := sessionFromContext(ctx.Context())
	if s.loginTokens == nil {
		return ctx.Status(http.StatusInternalServerError).Error("login tokens are not available")
	}
	// The posted value is not part of the audit row: a person can paste a
	// token into the date field, and that must not be stored.
	raw := strings.TrimSpace(ctx.PostFormValue("expires"))
	exp, err := logintoken.ParseExpiry(raw, s.loginTokens.Now(), time.Local)
	if err != nil {
		s.auditAction(ctx, audit.ActionLoginTokenCreate, errors.New("bad expiry"), nil)
		d := s.loginTokensPageData(ctx)
		d.LoginTokenRows = s.loginTokenRows(sess.DiscordUserID)
		if errors.Is(err, logintoken.ErrPastExpiry) {
			d.Error = "That expiry is already past."
		} else {
			d.Error = "Enter a valid expiry date, or leave it empty."
		}
		ctx.Status(http.StatusUnprocessableEntity)
		return s.viewPage(ctx, "login_tokens", d)
	}
	wire, rec, err := s.loginTokens.Mint(logintoken.MintOpts{
		OwnerID:   sess.DiscordUserID,
		ExpiresAt: exp,
	})
	if err != nil {
		s.auditAction(ctx, audit.ActionLoginTokenCreate, errors.New("mint failed"), nil)
		d := s.loginTokensPageData(ctx)
		d.LoginTokenRows = s.loginTokenRows(sess.DiscordUserID)
		d.Error = "Could not create a token."
		ctx.Status(http.StatusInternalServerError)
		return s.viewPage(ctx, "login_tokens", d)
	}
	detail := map[string]any{"tokenId": rec.ID}
	if !rec.ExpiresAt.IsZero() {
		detail["expiresAt"] = rec.ExpiresAt.Format(time.RFC3339)
	}
	s.auditAction(ctx, audit.ActionLoginTokenCreate, nil, detail)
	d := s.loginTokensPageData(ctx)
	d.LoginTokenRows = s.loginTokenRows(sess.DiscordUserID)
	d.LoginTokenSecret = wire
	return s.viewPage(ctx, "login_tokens", d)
}

func (s *Server) postLoginTokenRevoke(ctx *hime.Context) error {
	sess := sessionFromContext(ctx.Context())
	id := safeTokenID(ctx.PostFormValue("id"))
	if id == "" {
		s.auditAction(ctx, audit.ActionLoginTokenRevoke, errors.New("not found"), nil)
		return ctx.Status(http.StatusNotFound).Error("token not found")
	}
	err := s.loginTokens.Revoke(sess.DiscordUserID, id)
	if errors.Is(err, logintoken.ErrNotFound) || errors.Is(err, logintoken.ErrNotOwner) {
		s.auditAction(ctx, audit.ActionLoginTokenRevoke, errors.New("not found"), map[string]any{"tokenId": id})
		return ctx.Status(http.StatusNotFound).Error("token not found")
	}
	if err != nil {
		s.auditAction(ctx, audit.ActionLoginTokenRevoke, errors.New("revoke failed"), map[string]any{"tokenId": id})
		return ctx.Status(http.StatusInternalServerError).Error("could not revoke token")
	}
	s.auditAction(ctx, audit.ActionLoginTokenRevoke, nil, map[string]any{"tokenId": id})
	return ctx.RedirectTo("account.tokens")
}
