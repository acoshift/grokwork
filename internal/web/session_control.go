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
// units require owner/co-owner, and a web admin (the substitute for a Discord
// moderator) always passes. Pure ownership check — the feature+role gate is
// applied upstream by requireFeature/requireMember (and 404s in auth-off LAN
// mode before any handler runs).
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

// postSessionReset forgets the session, worktree, and branch. On success the
// unit no longer exists, so the redirect leaves the dead page for the project
// sessions list; a busy refusal keeps the user on the session page.
func (s *Server) postSessionReset(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	project, err := s.ensureThreadAccess(ctx, threadID)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	ent, _ := s.sessions.Get(threadID)
	if !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, audit.ActionSessionReset, errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error(errControlForbidden.Error())
	}
	// Capture the project before Reset deletes the entry (redirect target).
	if project == "" {
		project = strings.TrimSpace(ent.Project)
	}
	msg, resetErr := s.bot.ResetUnit(threadID)
	s.auditAction(ctx, audit.ActionSessionReset, resetErr, map[string]any{"threadId": threadID, "project": project})
	if resetErr != nil {
		// Busy refusal: the unit still exists — stay on the session page.
		return s.sessionRedirect(ctx, threadID, "", msg)
	}
	q := url.Values{}
	q.Set("ok", msg)
	if project != "" {
		return ctx.Redirect("/projects/" + url.PathEscape(project) + "/sessions?" + q.Encode())
	}
	return ctx.Redirect("/sessions?" + q.Encode())
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
	return s.sessionRedirect(ctx, threadID, "Label updated.", "")
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
	claimErr := s.bot.ClaimThread(threadID, actor)
	s.auditAction(ctx, audit.ActionSessionClaim, claimErr, map[string]any{"threadId": threadID})
	if claimErr != nil {
		return s.sessionRedirect(ctx, threadID, "", claimErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "You now own this session.", "")
}
