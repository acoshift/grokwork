package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// postPRAgentReview starts a new session that agentically reviews the pull request
// and posts its findings as one PR comment (the agent owns `gh pr comment`; unlike
// the commit-review dispatch it files no issues). Always a new session: a review is
// a fresh read of the current head, so reusing a session that already reasoned about
// an older diff would bias it. Distinct from postPRReview, which records a
// *human* team review verdict.
func (s *Server) postPRAgentReview(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prAddressRedirect(ctx, owner, repo, n, project, "", err, http.StatusFound)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionPRReviewStart, err, map[string]any{
			"project": project, "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	// Best-effort PR metadata for prompt context: a failed view still leaves the
	// agent enough (owner/repo/number) to fetch the PR itself.
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	detail, _ := ghpr.ViewPRDetailWith(ctx.Context(), s.ghRun(), cwd, selector)
	prURL := strings.TrimSpace(detail.URL)
	if prURL == "" {
		prURL = selector
	}

	actor := s.fixActor(ctx)
	model := strings.TrimSpace(ctx.PostFormValue("model"))
	res, startErr := s.bot.StartPRReview(bot.PRReviewOpts{
		Project: project, Actor: actor,
		Owner: owner, Repo: repo, Number: n,
		Title: detail.Title, URL: prURL, State: detail.State,
		HeadSHA: detail.HeadSHA, HeadRef: detail.HeadRef, BaseRef: detail.BaseRef,
		Body: detail.Body, Author: detail.Author,
		Additions: detail.Additions, Deletions: detail.Deletions, ChangedFiles: detail.ChangedFiles,
		Model: model,
	})

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo, "number": n, "model": model,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionPRReviewStart, startErr, detailMap)
		status := http.StatusBadRequest
		if errors.Is(startErr, bot.ErrQueueFull) {
			status = http.StatusConflict
		}
		return s.prAddressRedirect(ctx, owner, repo, n, project, "", startErr, status)
	}
	s.auditAction(ctx, audit.ActionPRReviewStart, nil, detailMap)

	// No DiscordOffline branch: a PR review is always web-native, so there is no
	// Discord destination to have promised and failed to deliver.
	return s.sessionRedirect(ctx, res.ThreadID, string(res.Status), "")
}
