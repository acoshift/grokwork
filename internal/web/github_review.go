package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// postPRGitHubReview submits a real GitHub review (`gh pr review`) as the
// authenticated gh user.
//
// Deliberately a separate route and a separate rail action from postPRReview
// (POST …/reviews), which records a grokwork-local team verdict: only this one
// reaches GitHub, and only this one can satisfy branch protection. Folding them
// together — one form with a "also send to GitHub" checkbox, say — is the exact
// confusion the team-review disclaimers exist to prevent, because a local
// verdict would then look interchangeable with a GitHub approval.
func (s *Server) postPRGitHubReview(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	verdict := ghpr.NormalizeReviewVerdict(ctx.PostFormValue("verdict"))

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	detailMap := map[string]any{
		"owner": owner, "repo": repo, "number": n, "project": project,
		"verdict": string(verdict),
	}

	// GithubWrites is the capability that already governs every write we make
	// with the host gh credential, and a review is one of them — the most
	// consequential, since an approval can be the last gate before merge. The
	// route's requireFeature gate is deployment-wide; this one is per project,
	// and it is enforced here rather than only by hiding the rail action.
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).GithubWrites {
		denied := errors.New("not allowed to submit GitHub reviews for this project")
		s.auditAction(ctx, audit.ActionPRReviewGitHub, denied, detailMap)
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	if verdict == "" {
		bad := errors.New("invalid review verdict")
		s.auditAction(ctx, audit.ActionPRReviewGitHub, bad, detailMap)
		return s.prRedirect(ctx, owner, repo, n, project, "", bad)
	}

	// Same attribution as a PR comment: GitHub files the review under the host
	// gh account, so the body is the only place the human behind it is named.
	body := s.attributeCommentBody(ctx, ctx.PostFormValue("body"))

	err = ghpr.SubmitReviewWith(ctx.Context(), s.ghRun(), cwd, owner, repo, n, verdict, body)
	s.auditAction(ctx, audit.ActionPRReviewGitHub, err, detailMap)
	if err != nil {
		// GitHub's refusal (self-approval, not a collaborator, PR closed) is the
		// whole answer; the alert modal keeps it from being lost under a flash.
		return s.prRedirectAlert(ctx, owner, repo, n, project, userFacingErr(err), "GitHub review failed")
	}
	return s.prRedirect(ctx, owner, repo, n, project, "GitHub review submitted ("+string(verdict)+")", nil)
}
