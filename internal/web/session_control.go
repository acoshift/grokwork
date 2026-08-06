package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// errControlForbidden is audited (and surfaced) when a caller may open the
// thread but is not authorized to cancel/reset it.
var errControlForbidden = errors.New("forbidden: not authorized to control this session")

// canControlSession is the web mirror of the Discord canControlThread gate for
// cancel/reset/dequeue: unowned units are soft-open to any project member, owned
// units require owner/co-owner, and a web admin always passes. That bypass is a
// web-auth role — who administers this server, so a host admin can always stop a
// run — and is deliberately not the Discord side's project-admin capability.
// Pure ownership check — the feature+role gate is applied upstream by
// requireFeature/requireMember (and 404s in auth-off LAN mode before any
// handler runs).
func (s *Server) canControlSession(ctx *hime.Context, ent sessionstore.Entry) bool {
	userID, role := s.sessionIdentity(ctx)
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		return true
	}
	if !ent.HasOwner() {
		return true
	}
	return ent.CanControl(userID)
}

// postSessionCancel stops the active run. Deliberately not rate-limited — a
// runaway run must always be stoppable.
func (s *Server) postSessionCancel(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	ent, _ := s.sessions.Get(threadID)
	if !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, audit.ActionSessionCancel, errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error(errControlForbidden.Error())
	}
	msg, ok := s.bot.CancelRun(threadID, s.fixActor(ctx).String())
	if !ok {
		s.auditAction(ctx, audit.ActionSessionCancel, errors.New(msg), map[string]any{"threadId": threadID})
		return s.sessionRedirect(ctx, threadID, "", msg)
	}
	s.auditAction(ctx, audit.ActionSessionCancel, nil, map[string]any{"threadId": threadID})
	return s.sessionRedirect(ctx, threadID, msg, "")
}

// postSessionReset is the web Abandon action (same ResetUnit core as Discord
// /reset): clear worktree/branch/Grok id, keep a tombstone labeled abandoned.
// On success stay on the session page with a flash; danger zone hides for
// terminal labels. Busy refusal keeps the page with an error flash.
func (s *Server) postSessionReset(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	project, err := s.ensureThreadAccess(ctx, threadID)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	ent, hasSession := s.sessions.Get(threadID)
	if !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, audit.ActionSessionReset, errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error(errControlForbidden.Error())
	}
	if project == "" {
		project = strings.TrimSpace(ent.Project)
	}
	// UI hides Abandon for terminal labels; refuse if POSTed anyway.
	if hasSession && sessionstore.IsTerminalLabel(ent.EffectiveLabel()) {
		return s.sessionRedirect(ctx, threadID, "", "Session is already done or abandoned.")
	}
	msg, resetErr := s.bot.ResetUnit(threadID)
	s.auditAction(ctx, audit.ActionSessionReset, resetErr, map[string]any{"threadId": threadID, "project": project})
	if resetErr != nil {
		return s.sessionRedirect(ctx, threadID, "", msg)
	}
	// Web-facing flash (Discord still uses ResetUnit's "Session was reset.").
	ok := "Session abandoned."
	// Stamp project for workspace scope (entry still exists as tombstone, but
	// keep the query for consistency with older clients).
	q := url.Values{}
	q.Set("ok", ok)
	if project != "" {
		q.Set("project", project)
	}
	return ctx.Redirect("/sessions/" + url.PathEscape(threadID) + "?" + q.Encode())
}

// postSessionQueueRemove drops one pending follow-up by taskID. Per-item
// permission (own item or canControl) is enforced inside RemoveQueuedTask.
func (s *Server) postSessionQueueRemove(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	taskID := strings.TrimSpace(ctx.PostFormValue("task_id"))
	if taskID == "" {
		return s.sessionRedirect(ctx, threadID, "", "missing task id")
	}
	ent, _ := s.sessions.Get(threadID)
	actor := s.fixActor(ctx)
	rmErr := s.bot.RemoveQueuedTask(threadID, taskID, actor.ID, s.canControlSession(ctx, ent))
	s.auditAction(ctx, audit.ActionSessionDequeue, rmErr, map[string]any{"threadId": threadID, "taskId": taskID})
	if rmErr != nil {
		return s.sessionRedirect(ctx, threadID, "", rmErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Removed queued follow-up.", "")
}

// postSessionLabel sets the lifecycle label. No ownership gate — mirrors Discord
// /label which is allowlist-only (feature+member is the gate).
func (s *Server) postSessionLabel(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	label := strings.TrimSpace(ctx.PostFormValue("label"))
	if label == "" {
		return s.sessionRedirect(ctx, threadID, "", "label is required")
	}
	setErr := s.bot.SetSessionLabel(threadID, label)
	s.auditAction(ctx, audit.ActionSessionLabel, setErr, map[string]any{"threadId": threadID, "label": label})
	if setErr != nil {
		return s.sessionRedirect(ctx, threadID, "", setErr.Error())
	}
	ok := "Label updated."
	if lab, parsed := sessionstore.ParseLabel(label); parsed && lab == sessionstore.LabelDone {
		ok = "Session marked as done."
	}
	return s.sessionRedirect(ctx, threadID, ok, "")
}

// postSessionGoal sets the sticky goal.
func (s *Server) postSessionGoal(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	goal := strings.TrimSpace(ctx.PostFormValue("goal"))
	setErr := s.bot.SetSessionGoal(threadID, goal)
	s.auditAction(ctx, audit.ActionSessionGoal, setErr, map[string]any{"threadId": threadID})
	if setErr != nil {
		return s.sessionRedirect(ctx, threadID, "", setErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Goal updated.", "")
}

// postSessionClaim takes over ownership. This is the lockout-breaker that makes
// cancel/reset usable for web users on units they did not start; any member may
// claim (feature+member gate), so it is deliberately not behind canControlSession.
// Terminal units (done/abandoned) cannot be claimed — there is nothing to control.
func (s *Server) postSessionClaim(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	actor := s.fixActor(ctx)
	if strings.TrimSpace(actor.ID) == "" {
		// No OAuth identity to assign (auth-off LAN mode has none).
		err := errors.New("claim requires a signed-in identity")
		s.auditAction(ctx, audit.ActionSessionClaim, err, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	if ent, ok := s.sessions.Get(threadID); ok && sessionstore.IsTerminalLabel(ent.EffectiveLabel()) {
		err := errors.New("cannot claim a done or abandoned session")
		s.auditAction(ctx, audit.ActionSessionClaim, err, map[string]any{"threadId": threadID})
		return s.sessionRedirect(ctx, threadID, "", err.Error())
	}
	claimErr := s.bot.ClaimThread(threadID, actor)
	s.auditAction(ctx, audit.ActionSessionClaim, claimErr, map[string]any{"threadId": threadID})
	if claimErr != nil {
		return s.sessionRedirect(ctx, threadID, "", claimErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "You now own this session.", "")
}
