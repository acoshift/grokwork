package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// caseByKey resolves /c/{key} and /projects/{project}/cases/{key} to the case's
// session page.
//
// A redirect rather than a second rendering of the case: the key is a
// *reference*, and a second URL that also rendered the case would mean two
// things to bookmark, two shell scopes to derive, and a second place for every
// session-page affordance to be kept correct. It resolves, then hands over to
// the one page that owns cases.
func (s *Server) caseByKey(ctx *hime.Context) error {
	key := sessionstore.NormalizeCaseKey(ctx.PathValue("key"))
	if key == "" {
		return ctx.Status(http.StatusNotFound).Error("not a case id")
	}
	threadID, ent, ok := s.bot.FindCaseByKey(key)
	// Access is checked as the session page checks it, and a denial answers
	// exactly as a miss does — otherwise the key space becomes a probe for
	// which projects and cases exist.
	if !ok {
		return ctx.Status(http.StatusNotFound).Error("no case " + key)
	}
	if ent.Project != "" {
		if err := s.ensureProjectAccess(ctx, ent.Project); err != nil {
			return ctx.Status(http.StatusNotFound).Error("no case " + key)
		}
	}
	loc := "/sessions/" + url.PathEscape(threadID)
	if ent.Project != "" {
		loc += "?project=" + url.QueryEscape(ent.Project)
	}
	return ctx.Redirect(loc)
}

// postCaseLink records that this case relates to another. Gated on the same
// capability as filing a case — noticing that two reports share a root cause is
// support work, not a lifecycle change.
func (s *Server) postCaseLink(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if canOpen, _, _ := s.resolveCaseCaps(ctx, ent.Project); !canOpen && !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, audit.ActionCaseLink, errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to link cases")
	}
	key := strings.TrimSpace(ctx.PostFormValue("caseKey"))
	linkErr := s.bot.LinkCase(threadID, key)
	s.auditAction(ctx, audit.ActionCaseLink, linkErr, map[string]any{"threadId": threadID, "caseKey": key})
	if linkErr != nil {
		return s.sessionRedirect(ctx, threadID, "", linkErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Linked "+sessionstore.NormalizeCaseKey(key)+".", "")
}

// postCaseUnlink drops one outbound reference. Inbound references belong to the
// case that made them and are not removable here.
func (s *Server) postCaseUnlink(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if canOpen, _, _ := s.resolveCaseCaps(ctx, ent.Project); !canOpen && !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, audit.ActionCaseUnlink, errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to link cases")
	}
	key := strings.TrimSpace(ctx.PostFormValue("caseKey"))
	unlinkErr := s.bot.UnlinkCase(threadID, key)
	s.auditAction(ctx, audit.ActionCaseUnlink, unlinkErr, map[string]any{"threadId": threadID, "caseKey": key})
	if unlinkErr != nil {
		return s.sessionRedirect(ctx, threadID, "", unlinkErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Removed case link.", "")
}
