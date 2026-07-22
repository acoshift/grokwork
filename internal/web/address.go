package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/ghpr"
)

func (s *Server) postPRAddressCI(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	forceNew := formBool(ctx.PostFormValue("force_new"))
	pickThread := strings.TrimSpace(ctx.PostFormValue("thread_id"))

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prAddressRedirect(ctx, owner, repo, n, project, "", err, http.StatusFound)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"kind": "address_ci", "project": project, "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	// Best-effort PR + checks for prompt context.
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	info, _ := ghpr.ViewWith(ctx.Context(), s.ghRun(), cwd, selector)
	checks, _ := ghpr.ListChecksWith(ctx.Context(), s.ghRun(), cwd, selector)
	failed := ghpr.FailedChecks(checks)
	snippet := ""
	if info.HeadRef != "" || info.HeadSHA != "" {
		branch := info.HeadRef
		snippet = ghpr.FailedLogSnippetWith(ctx.Context(), s.ghRun(), cwd, branch, info.HeadSHA, 4000)
	}
	if info.Number == 0 {
		info.Number = n
		info.Owner, info.Repo = owner, repo
		info.URL = selector
	}

	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartAddressCI(bot.AddressCIOpts{
		Project: project, Actor: actor, ForceNew: forceNew, ThreadID: pickThread,
		Owner: owner, Repo: repo, Number: n,
		Title: info.Title, URL: info.URL, State: info.State,
		HeadSHA: info.HeadSHA, HeadRef: info.HeadRef, Checks: info.Checks,
		Failed: failed, LogSnippet: snippet,
	})
	return s.handleAddressResult(ctx, startErr, res, addressRedirectContext{
		Kind: "address_ci", Project: project, Owner: owner, Repo: repo, Number: n,
	})
}

func (s *Server) postPRAddressReview(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	forceNew := formBool(ctx.PostFormValue("force_new"))
	pickThread := strings.TrimSpace(ctx.PostFormValue("thread_id"))

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prAddressRedirect(ctx, owner, repo, n, project, "", err, http.StatusFound)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"kind": "address_review", "project": project, "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	comments, listErr := ghpr.ListUnresolvedReviewCommentsWith(ctx.Context(), s.ghRun(), cwd, owner, repo, n)
	if listErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, listErr, map[string]any{
			"kind": "address_review", "project": project, "owner": owner, "repo": repo, "number": n,
		})
		return s.prAddressRedirect(ctx, owner, repo, n, project, "",
			fmt.Errorf("could not list review comments: %w", listErr), http.StatusBadRequest)
	}
	if len(comments) == 0 {
		err := bot.ErrNoReviewComments
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"kind": "address_review", "project": project, "owner": owner, "repo": repo, "number": n,
		})
		return s.prAddressRedirect(ctx, owner, repo, n, project, "", err, http.StatusBadRequest)
	}

	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	info, _ := ghpr.ViewWith(ctx.Context(), s.ghRun(), cwd, selector)
	title := info.Title
	prURL := info.URL
	if prURL == "" {
		prURL = selector
	}

	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartAddressReview(bot.AddressReviewOpts{
		Project: project, Actor: actor, ForceNew: forceNew, ThreadID: pickThread,
		Owner: owner, Repo: repo, Number: n, Title: title, URL: prURL,
		Comments: comments,
	})
	return s.handleAddressResult(ctx, startErr, res, addressRedirectContext{
		Kind: "address_review", Project: project, Owner: owner, Repo: repo, Number: n,
	})
}

func (s *Server) postSessionContinue(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	project, err := s.ensureThreadAccess(ctx, threadID)
	if err != nil {
		return forbiddenProject(ctx, err)
	}
	prompt := strings.TrimSpace(ctx.PostFormValue("prompt"))
	if prompt == "" {
		return s.sessionRedirect(ctx, threadID, "", "prompt is required")
	}
	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"kind": "continue", "threadId": threadID,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}
	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartContinue(bot.ContinueOpts{
		ThreadID: threadID, Project: project, Prompt: prompt, Actor: actor,
	})
	detail := map[string]any{"kind": "continue", "threadId": threadID, "project": project}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		if errors.Is(startErr, bot.ErrQueueFull) {
			return ctx.Status(http.StatusConflict).Error(startErr.Error())
		}
		if errors.Is(startErr, bot.ErrUnknownThread) {
			return ctx.Status(http.StatusNotFound).Error(startErr.Error())
		}
		return s.sessionRedirect(ctx, threadID, "", startErr.Error())
	}
	detail["status"] = string(res.Status)
	detail["queuePos"] = res.QueuePos
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)
	ok := string(res.Status)
	if res.DiscordOffline {
		ok += "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

type addressRedirectContext struct {
	Kind    string
	Project string
	Owner   string
	Repo    string
	Number  int
}

func (s *Server) handleAddressResult(ctx *hime.Context, startErr error, res bot.FixStartResult, rc addressRedirectContext) error {
	detail := map[string]any{
		"kind": rc.Kind, "project": rc.Project,
		"owner": rc.Owner, "repo": rc.Repo, "number": rc.Number,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if errors.Is(startErr, bot.ErrPickerRequired) {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		q := url.Values{}
		q.Set("picker", "1")
		q.Set("err", "Multiple sessions bind this PR — pick one or force a new thread.")
		if rc.Project != "" {
			q.Set("project", rc.Project)
		}
		loc := fmt.Sprintf("/prs/%s/%s/%d?%s", url.PathEscape(rc.Owner), url.PathEscape(rc.Repo), rc.Number, q.Encode())
		return ctx.Redirect(loc)
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		status := http.StatusBadRequest
		switch {
		case errors.Is(startErr, bot.ErrDiscordNotReady):
			status = http.StatusServiceUnavailable
		case errors.Is(startErr, bot.ErrQueueFull):
			status = http.StatusConflict
		}
		if status == http.StatusServiceUnavailable || status == http.StatusConflict {
			return ctx.Status(status).Error(startErr.Error())
		}
		return s.prAddressRedirect(ctx, rc.Owner, rc.Repo, rc.Number, rc.Project, "", startErr, http.StatusFound)
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)
	ok := string(res.Status)
	if res.DiscordOffline {
		ok += "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) prAddressRedirect(ctx *hime.Context, owner, repo string, n int, project, ok string, err error, status int) error {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if ok != "" {
		q.Set("ok", ok)
	}
	if err != nil {
		q.Set("err", err.Error())
	}
	loc := fmt.Sprintf("/prs/%s/%s/%d", url.PathEscape(owner), url.PathEscape(repo), n)
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	if status == http.StatusBadRequest {
		// Prefer redirect with flash for browser UX when not a hard API status.
		return ctx.Redirect(loc)
	}
	if status != http.StatusFound && status != http.StatusSeeOther && status != 0 {
		return ctx.Status(status).Error(q.Get("err"))
	}
	return ctx.Redirect(loc)
}
