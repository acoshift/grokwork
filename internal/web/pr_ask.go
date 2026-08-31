package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const prAskPatchCap = 80_000

// postPRAsk starts or continues a throwaway in-page Q&A about this PR.
// Stays on the PR page. Distinct from postPRAgentReview (which posts a GitHub
// comment and redirects to a listed session).
func (s *Server) postPRAsk(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	question := strings.TrimSpace(ctx.PostFormValue("prompt"))
	if question == "" {
		return s.prAskRedirect(ctx, owner, repo, n, project, "", fmt.Errorf("prompt is required"), http.StatusFound)
	}

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prAskRedirect(ctx, owner, repo, n, project, "", err, http.StatusFound)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkStartRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionPRAskStart, err, map[string]any{
			"project": project, "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	detail, _ := ghpr.ViewPRDetailWith(ctx.Context(), s.ghRun(), cwd, selector)
	prURL := strings.TrimSpace(detail.URL)
	if prURL == "" {
		prURL = selector
	}
	diff := ""
	if patch, err := ghpr.PRPatchWith(ctx.Context(), s.ghRun(), cwd, selector); err == nil {
		diff = truncateRunes(string(patch), prAskPatchCap)
	}

	actor := s.fixActor(ctx)
	model := strings.TrimSpace(ctx.PostFormValue("model"))
	res, startErr := s.bot.StartPRAsk(bot.PRAskOpts{
		Project: project, Actor: actor,
		Owner: owner, Repo: repo, Number: n,
		Title: detail.Title, URL: prURL, State: detail.State,
		HeadSHA: detail.HeadSHA, HeadRef: detail.HeadRef, BaseRef: detail.BaseRef,
		Body: detail.Body, Author: detail.Author,
		Additions: detail.Additions, Deletions: detail.Deletions, ChangedFiles: detail.ChangedFiles,
		Diff: diff, Question: question, Model: model,
	})

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo, "number": n, "model": model,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionPRAskStart, startErr, detailMap)
		status := http.StatusBadRequest
		if errors.Is(startErr, bot.ErrQueueFull) {
			status = http.StatusConflict
		}
		return s.prAskRedirect(ctx, owner, repo, n, project, "", startErr, status)
	}
	s.auditAction(ctx, audit.ActionPRAskStart, nil, detailMap)
	ok := "Question sent"
	if res.Status == bot.FixStatusQueued {
		ok = "Question queued"
	}
	return s.prAskRedirect(ctx, owner, repo, n, project, ok, nil, http.StatusSeeOther)
}

func (s *Server) prAskRedirect(ctx *hime.Context, owner, repo string, n int, project, ok string, err error, status int) error {
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
	loc += "#pr-ask"
	if status == http.StatusBadRequest {
		return ctx.Redirect(loc)
	}
	if status != http.StatusFound && status != http.StatusSeeOther && status != 0 {
		return ctx.Status(status).Error(q.Get("err"))
	}
	return ctx.Redirect(loc)
}

func (s *Server) partialPRAsk(ctx *hime.Context) error {
	d, err := s.prAskPageData(ctx)
	if err != nil {
		return err
	}
	return s.viewFragment(ctx, "pr_detail", "pr_ask", d)
}

func (s *Server) partialPRAskRun(ctx *hime.Context) error {
	d, err := s.prAskPageData(ctx)
	if err != nil {
		return err
	}
	return s.viewFragment(ctx, "pr_detail", "pr_ask_run", d)
}

// prAskPageData is the live-region payload for in-page Q&A. It must not call
// gh pr view: fpDashboard includes elapsed, so this path runs about every 2s
// while an ask is streaming.
func (s *Server) prAskPageData(ctx *hime.Context) (pageData, error) {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 || owner == "" || repo == "" {
		return pageData{}, ctx.Status(http.StatusBadRequest).Error("invalid PR path")
	}
	project := strings.TrimSpace(ctx.FormValue("project"))
	project, ref, _, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return pageData{}, ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	d := s.basePage(ctx)
	d.IsShip = true
	d.Project = project
	d.ActiveOwner = ref.Owner
	d.ActiveRepo = ref.Repo
	d.PRNumber = n
	s.attachPRAsk(&d, project, ref.Owner, ref.Repo, n)
	return d, nil
}

func (s *Server) attachPRAsk(d *pageData, project, owner, repo string, n int) {
	if d == nil || s.bot == nil || project == "" {
		return
	}
	askKey := sessionstore.FormatAskPRKey(owner, repo, n)
	tid := s.bot.FindPRAsk(project, askKey, d.UserID)
	if tid == "" {
		return
	}
	d.PRAskThreadID = tid
	d.ThreadID = tid
	d.AgentLabel = s.agentLabel(tid)
	if s.history != nil {
		if th, err := s.history.Get(tid); err == nil {
			d.Thread = th
		}
	}
	if s.bot != nil {
		snap := s.bot.StatusSnapshot()
		for _, r := range snap.ActiveRuns {
			if r.ThreadID == tid {
				d.RunActivity = r.Activity
				d.RunPhases = r.Phases
				d.RunElapsed = r.Elapsed
				d.RunBusy = true
				d.RunQueue = r.QueueLen
				d.RunPrompt = r.Prompt
				d.RunLiveText = r.LiveText
				break
			}
		}
	}
}

func (s *Server) redirectIfPRAsk(ctx *hime.Context, threadID string) (handled bool, err error) {
	if s.sessions == nil {
		return false, nil
	}
	e, ok := s.sessions.Get(threadID)
	if !ok || !e.IsPRAsk() {
		return false, nil
	}
	owner, repo, n, parsed := sessionstore.ParseAskPRKey(e.AskPRKey)
	if !parsed {
		return true, ctx.Status(http.StatusNotFound).Error("not found")
	}
	project := strings.TrimSpace(e.Project)
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	loc := fmt.Sprintf("/prs/%s/%s/%d", url.PathEscape(owner), url.PathEscape(repo), n)
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return true, ctx.Redirect(loc + "#pr-ask")
}

func (s *Server) refusePRAskPartial(ctx *hime.Context, threadID string) bool {
	if s.sessions == nil {
		return false
	}
	e, ok := s.sessions.Get(threadID)
	return ok && e.IsPRAsk()
}

func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	var b strings.Builder
	b.Grow(n)
	i := 0
	for _, r := range s {
		if i >= n {
			break
		}
		b.WriteRune(r)
		i++
	}
	return b.String() + "\n…(truncated)"
}
