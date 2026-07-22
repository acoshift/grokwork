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

// startComposer renders the "Start a task" page: the web equivalent of tagging
// @Grok in a mapped Discord channel. The page renders read-only when the viewer
// cannot start sessions; the hard gate lives on the POST (requireFeature +
// requireMember).
func (s *Server) startComposer(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.basePage(ctx)
	d.Title = project + " · Start task"
	d.IsStart = true
	d.Project = project
	d.StartDirectShip = s.cfg.ProjectDirectToPrimary(project)
	d.StartDiscordDest = s.startOpensDiscordThread(project)
	d.StartDefaultMode = s.cfg.ProjectDefaultMode(project)
	if d.StartDefaultMode == "" {
		d.StartDefaultMode = "fix"
	}
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	return s.viewPage(ctx, "start", d)
}

// startOpensDiscordThread reports whether a web start for the project would open
// a Discord thread (gateway up + exactly one mapped channel) rather than fall
// back to a web-native unit. Read-only hint only — StartWebTask re-decides.
func (s *Server) startOpensDiscordThread(project string) bool {
	if s.bot == nil || !s.bot.DiscordReady() {
		return false
	}
	_, err := s.cfg.PreferDiscordChannel(project)
	return err == nil
}

// postStart creates a new work unit from a freeform prompt and redirects to the
// live session page. Mirrors postSessionContinue's error shape; ErrQueueFull is
// impossible on create (a fresh unit has an empty queue).
func (s *Server) postStart(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	prompt := strings.TrimSpace(ctx.PostFormValue("prompt"))
	title := strings.TrimSpace(ctx.PostFormValue("title"))
	mode := strings.TrimSpace(ctx.PostFormValue("mode"))
	if prompt == "" {
		return s.startRedirect(ctx, project, "", "prompt is required")
	}
	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "origin": "web-start", "mode": mode,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartWebTask(bot.StartWebTaskOpts{
		Project: project, Prompt: prompt, Actor: actor, Title: title, Mode: mode,
	})
	detail := map[string]any{
		"project": project, "origin": "web-start", "mode": mode,
		"threadId": res.ThreadID, "status": string(res.Status), "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		if errors.Is(startErr, bot.ErrQueueFull) {
			return ctx.Status(http.StatusConflict).Error(startErr.Error())
		}
		return s.startRedirect(ctx, project, "", startErr.Error())
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)

	ok := string(res.Status)
	if res.DiscordOffline {
		ok += "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) startRedirect(ctx *hime.Context, project, ok, errMsg string) error {
	q := url.Values{}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	loc := "/projects/" + url.PathEscape(project) + "/start"
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}
