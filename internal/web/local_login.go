package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

// postLocalLogin authenticates an operator-provisioned account that has no
// Discord identity — the login path for someone who does not use Discord at all.
//
// Mirrors the Discord callback: authenticate, resolve a role, refuse if the role
// resolver says no, then create exactly the same kind of web session. The only
// difference is the actor id shape ("local:alice" rather than a snowflake), which
// the rest of the system already handles because ids are namespaced.
func (s *Server) postLocalLogin(ctx *hime.Context) error {
	if !s.cfg.WebAuthEnabled() {
		// With auth off the whole UI is open LAN mode; a login form would imply a
		// protection that does not exist.
		return ctx.Status(http.StatusNotFound).Error("web auth is disabled")
	}
	next := safeLocalNext(ctx.FormValue("next"))
	id := strings.TrimSpace(ctx.PostFormValue("username"))
	password := ctx.PostFormValue("password")

	account, err := s.cfg.AuthenticateLocal(id, password)
	if err != nil {
		// One message for every rejection reason: naming which part failed would
		// let anyone enumerate the provisioned accounts.
		s.auditLogin(audit.ActorAnonymous, "", false, "local login failed")
		if !errors.Is(err, config.ErrLocalAuthFailed) {
			return ctx.RedirectTo("login", map[string]string{"err": "sign-in failed"})
		}
		return ctx.RedirectTo("login", map[string]string{"err": "invalid credentials"})
	}

	actorID := account.ActorID()
	role := account.Role
	if role == config.WebRoleNone {
		// No explicit role on the account: fall through to the same lists and
		// project membership Discord logins use. The actor id is namespaced, so
		// "local:alice" in an allowlist resolves here.
		resolved, ok := s.cfg.ResolveWebRoleForConfig(actorID)
		if !ok {
			s.auditLogin(actorID, "", false, "not authorized")
			return ctx.RedirectTo("login", map[string]string{"err": "not authorized for this Grok Work instance"})
		}
		role = resolved
	}

	name := strings.TrimSpace(account.DisplayName)
	if name == "" {
		name = account.ID
	}
	sess, err := s.webSessions.Create(actorID, name, "", role)
	if err != nil {
		s.auditLogin(actorID, string(role), false, "session create failed")
		return ctx.Status(http.StatusInternalServerError).Error("session: " + err.Error())
	}
	if s.webUsers != nil {
		// No avatar: a local account has no upstream profile picture, and the shell
		// falls back to initials.
		_ = s.webUsers.Upsert(actorID, name, "")
	}
	s.auditLogin(actorID, string(role), true, "")
	s.SetSessionCookie(ctx.ResponseWriter(), sess.ID)
	return ctx.Redirect(next)
}
