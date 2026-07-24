package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func (s *Server) resolveCaseCaps(ctx *hime.Context, project string) (canOpen, canEscalate, canDraft bool) {
	caps := s.cfg.ResolveCapabilities(project, s.fixActor(ctx).ID, nil)
	return caps.Investigate || caps.FileEscalation || caps.StartSessions,
		bot.CanEscalateCaseCaps(caps),
		bot.CanDraftCaseCaps(caps)
}

func (s *Server) canReopenCase(ctx *hime.Context, ent sessionstore.Entry) bool {
	if !ent.IsCase() || !ent.IsCaseClosed() {
		return false
	}
	if s.canControlSession(ctx, ent) {
		return true
	}
	caps := s.cfg.ResolveCapabilities(ent.Project, s.fixActor(ctx).ID, nil)
	return bot.CanReopenCaseCaps(caps)
}

func (s *Server) postCaseEscalate(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseClosed.Error())
	}
	_, canEsc, _ := s.resolveCaseCaps(ctx, ent.Project)
	if !canEsc {
		s.auditAction(ctx, "case.escalate", bot.ErrCaseForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to escalate cases")
	}
	note := strings.TrimSpace(ctx.PostFormValue("note"))
	actor := s.fixActor(ctx)
	escErr := s.bot.EscalateCase(threadID, actor.ID, note)
	s.auditAction(ctx, "case.escalate", escErr, map[string]any{"threadId": threadID, "project": ent.Project})
	if escErr != nil {
		return s.sessionRedirect(ctx, threadID, "", escErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Escalated → fixing (Mode stays case).", "")
}

func (s *Server) postCaseAnswer(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseClosed.Error())
	}
	_, _, canDraft := s.resolveCaseCaps(ctx, ent.Project)
	if !canDraft {
		s.auditAction(ctx, "case.answer", bot.ErrCaseForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to mark cases answered")
	}
	note := strings.TrimSpace(ctx.PostFormValue("note"))
	actor := s.fixActor(ctx)
	ansErr := s.bot.AnswerCase(threadID, actor.ID, note)
	s.auditAction(ctx, "case.answer", ansErr, map[string]any{"threadId": threadID})
	if ansErr != nil {
		return s.sessionRedirect(ctx, threadID, "", ansErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Phase → answered. Set customer text then close when done.", "")
}

func (s *Server) postCaseClose(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseClosed.Error())
	}
	if !s.canControlSession(ctx, ent) {
		s.auditAction(ctx, "case.close", errControlForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error(errControlForbidden.Error())
	}
	res := strings.TrimSpace(ctx.PostFormValue("resolution"))
	note := strings.TrimSpace(ctx.PostFormValue("note"))
	actor := s.fixActor(ctx)
	closeErr := s.bot.CloseCase(threadID, actor.ID, res, note)
	s.auditAction(ctx, "case.close", closeErr, map[string]any{
		"threadId": threadID, "resolution": res,
	})
	if closeErr != nil {
		return s.sessionRedirect(ctx, threadID, "", closeErr.Error())
	}
	return s.sessionRedirect(ctx, threadID, "Case closed.", "")
}

func (s *Server) postCaseCustomerUpdate(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseClosed.Error())
	}
	_, _, canDraft := s.resolveCaseCaps(ctx, ent.Project)
	if !canDraft {
		s.auditAction(ctx, "case.customer_update", bot.ErrCaseForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to draft customer updates")
	}
	text := strings.TrimSpace(ctx.PostFormValue("text"))
	if text == "" {
		return s.sessionRedirect(ctx, threadID, "", "customer update text is required")
	}
	clean, hits, setErr := s.bot.SetCaseCustomerUpdate(threadID, text)
	s.auditAction(ctx, "case.customer_update", setErr, map[string]any{
		"threadId": threadID, "redacted": hits, "len": len(clean),
	})
	if setErr != nil {
		return s.sessionRedirect(ctx, threadID, "", setErr.Error())
	}
	msg := "Customer update saved"
	if len(hits) > 0 {
		msg += " (redacted: " + strings.Join(hits, ", ") + ")"
	}
	return s.sessionRedirect(ctx, threadID, msg, "")
}

func (s *Server) postCaseReopen(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if !ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseNotClosed.Error())
	}
	if !s.canReopenCase(ctx, ent) {
		s.auditAction(ctx, "case.reopen", bot.ErrCaseForbidden, map[string]any{"threadId": threadID})
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to reopen cases")
	}
	phase := strings.TrimSpace(ctx.PostFormValue("phase"))
	actor := s.fixActor(ctx)
	reopenErr := s.bot.ReopenCase(threadID, actor.ID, phase)
	s.auditAction(ctx, "case.reopen", reopenErr, map[string]any{
		"threadId": threadID, "phase": phase, "project": ent.Project,
	})
	if reopenErr != nil {
		return s.sessionRedirect(ctx, threadID, "", reopenErr.Error())
	}
	if phase == "" {
		phase = sessionstore.PhaseInvestigate
	}
	return s.sessionRedirect(ctx, threadID, "Case reopened · phase "+phase+".", "")
}

func (s *Server) postCaseInvestigate(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, err := s.loadCaseThread(ctx, threadID)
	if err != nil {
		return err
	}
	if ent.IsCaseClosed() {
		return s.sessionRedirect(ctx, threadID, "", bot.ErrCaseClosed.Error())
	}
	canOpen, _, _ := s.resolveCaseCaps(ctx, ent.Project)
	if !canOpen && !s.canControlSession(ctx, ent) {
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to investigate this case")
	}
	notes := strings.TrimSpace(ctx.PostFormValue("notes"))
	if notes == "" {
		notes = "Investigate this case further."
	}
	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartContinue(bot.ContinueOpts{
		ThreadID: threadID,
		Project:  ent.Project,
		Prompt:   notes,
		Actor:    actor,
	})
	s.auditAction(ctx, audit.ActionSessionStart, startErr, map[string]any{
		"threadId": threadID, "origin": "web-case-investigate",
		"status": string(res.Status),
	})
	if startErr != nil {
		if errors.Is(startErr, bot.ErrQueueFull) {
			return ctx.Status(http.StatusConflict).Error(startErr.Error())
		}
		return s.sessionRedirect(ctx, threadID, "", startErr.Error())
	}
	ok := "Investigate started"
	if res.Status == bot.FixStatusQueued {
		ok = "Investigate queued"
	}
	return s.sessionRedirect(ctx, threadID, ok, "")
}

// loadCaseThread enforces access and that the unit is Mode=case.
func (s *Server) loadCaseThread(ctx *hime.Context, threadID string) (sessionstore.Entry, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return sessionstore.Entry{}, ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return sessionstore.Entry{}, forbiddenProject(ctx, err)
	}
	ent, ok := s.sessions.Get(threadID)
	if !ok {
		return sessionstore.Entry{}, ctx.Status(http.StatusNotFound).Error("session not found")
	}
	if !ent.IsCase() {
		return sessionstore.Entry{}, ctx.Status(http.StatusBadRequest).Error("this session is not a case")
	}
	return ent, nil
}
