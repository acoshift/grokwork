package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
)

// caseNewPage renders the case intake form, the web entry point for a support
// case (the Discord equivalent is "@Grok /case [severity] [ref:ID] <title>" in
// a mapped channel, but neither surface is primary). The page
// renders read-only when the viewer cannot open cases; the hard gate lives on
// the POST (requireFeature + requireMember + per-project capability).
func (s *Server) caseNewPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.basePage(ctx)
	d.Title = project + " · New case"
	d.IsCases = true
	d.Project = project
	d.CanOpenCase = s.canOpenCase(d, project)
	d.StartDiscordDest = s.startOpensDiscordThread(project)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	return s.viewPage(ctx, "case_new", d)
}

// canOpenCase mirrors the Discord /case gate (case_cmd.go): investigator-class
// project capability is enough — GithubWrites is never required. Folded into
// the web feature+role gate (CanStartSession) so the form and board CTAs never
// render when the POST would 404/403.
func (s *Server) canOpenCase(d pageData, project string) bool {
	if !d.CanStartSession {
		return false
	}
	caps := s.cfg.ResolveCapabilities(project, d.UserID)
	return caps.Investigate || caps.FileEscalation || caps.StartSessions
}

// postCaseNew creates the case shell (Mode=case, Phase=intake) and redirects to
// the session workspace. Notes or images queue an investigate run; empty intake
// stays intake-only, exactly like Discord "/case".
func (s *Server) postCaseNew(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	// Multipart must be parsed before any PostFormValue (default limits).
	uploads, upErr := s.formImageUploads(ctx)
	if upErr != nil {
		return s.caseNewRedirect(ctx, project, upErr.Error())
	}
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	actor := s.fixActor(ctx)
	caps := s.cfg.ResolveCapabilities(project, actor.ID)
	if !caps.Investigate && !caps.FileEscalation && !caps.StartSessions {
		return ctx.Status(http.StatusForbidden).Error("forbidden: not allowed to open cases on this project")
	}
	title := strings.TrimSpace(ctx.PostFormValue("title"))
	severity := strings.TrimSpace(ctx.PostFormValue("severity"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	notes := strings.TrimSpace(ctx.PostFormValue("notes"))
	if title == "" {
		return s.caseNewRedirect(ctx, project, "customer-facing title is required")
	}
	if err := s.checkStartRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "origin": "web-case", "severity": severity,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	paths, cleanup, stageErr := s.bot.SaveWebAttachments(uploads)
	if stageErr != nil {
		return s.caseNewRedirect(ctx, project, stageErr.Error())
	}
	investigate := notes != "" || len(paths) > 0
	res, startErr := s.bot.StartCase(bot.StartCaseOpts{
		Project: project, Title: title, Severity: severity, Ref: ref, Notes: notes, Actor: actor,
		AttachmentPaths: paths,
	})
	detail := map[string]any{
		"project": project, "origin": "web-case", "severity": severity,
		"ref": ref, "investigate": investigate, "attachments": len(paths),
		"threadId": res.ThreadID, "status": string(res.Status), "created": res.Created,
	}
	if startErr != nil {
		cleanup()
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		if errors.Is(startErr, bot.ErrQueueFull) {
			return ctx.Status(http.StatusConflict).Error(startErr.Error())
		}
		return s.caseNewRedirect(ctx, project, startErr.Error())
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)

	ok := "Case opened"
	if investigate {
		ok = "Case opened · investigating"
		if res.Status == bot.FixStatusQueued {
			ok = "Case opened · investigate queued"
		}
	}
	if res.DiscordOffline {
		ok += "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) caseNewRedirect(ctx *hime.Context, project, errMsg string) error {
	q := url.Values{}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	loc := "/projects/" + url.PathEscape(project) + "/cases/new"
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}
