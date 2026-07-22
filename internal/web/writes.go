package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// requireFeature rejects when the named write feature is off (404).
func (s *Server) requireFeature(feature string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		on := false
		switch feature {
		case "githubWrites":
			on = s.cfg.FeatureGitHubWrites()
		case "merge":
			on = s.cfg.FeatureMerge()
		case "startSessions":
			on = s.cfg.FeatureStartSessions()
		case "prReviews":
			on = s.cfg.FeaturePRReviews()
		}
		if !on {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireMember requires member+ role + CSRF when auth is on; when auth off, feature gate alone applies.
func (s *Server) requireMember(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.WebAuthEnabled() {
			// Features are off when auth is off (Feature* false), so this is rare.
			next.ServeHTTP(w, r)
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil {
			sess = s.sessionFromRequest(r)
		}
		if sess == nil || !config.RoleAtLeast(sess.Role, config.WebRoleMember) {
			http.Error(w, "forbidden: member required", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if !s.checkCSRF(r, sess) {
				http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	}))
}

func (s *Server) postIssueComment(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	body := ctx.PostFormValue("body")
	project, ref, path, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.issueRedirect(ctx, project, owner, repo, n, "", err)
	}
	owner, repo = ref.Owner, ref.Repo
	err = ghpr.CommentIssueWith(ctx.Context(), s.ghRun(), path, owner, repo, n, body)
	s.auditAction(ctx, audit.ActionIssueComment, err, map[string]any{
		"project": project, "owner": owner, "repo": repo, "number": n,
	})
	if err != nil {
		return s.issueRedirect(ctx, project, owner, repo, n, "", err)
	}
	s.invalidateIssueListCache(project, owner, repo)
	return s.issueRedirect(ctx, project, owner, repo, n, "Comment posted", nil)
}

func (s *Server) postIssueClose(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	body := ctx.PostFormValue("body")
	if strings.TrimSpace(body) == "" {
		return s.issueRedirect(ctx, project, owner, repo, n, "", fmt.Errorf("comment body required to close"))
	}
	project, ref, path, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.issueRedirect(ctx, project, owner, repo, n, "", err)
	}
	owner, repo = ref.Owner, ref.Repo
	err = ghpr.CloseIssueWith(ctx.Context(), s.ghRun(), path, owner, repo, n, body)
	s.auditAction(ctx, audit.ActionIssueClose, err, map[string]any{
		"project": project, "owner": owner, "repo": repo, "number": n, "withComment": true,
	})
	if err != nil {
		return s.issueRedirect(ctx, project, owner, repo, n, "", err)
	}
	s.invalidateIssueListCache(project, owner, repo)
	return s.issueRedirect(ctx, project, owner, repo, n, "Issue closed", nil)
}

func (s *Server) postPRComment(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	body := ctx.PostFormValue("body")
	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo
	err = ghpr.CommentPRWith(ctx.Context(), s.ghRun(), cwd, owner, repo, n, body)
	s.auditAction(ctx, audit.ActionPRComment, err, map[string]any{
		"owner": owner, "repo": repo, "number": n, "project": project,
	})
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	return s.prRedirect(ctx, owner, repo, n, project, "Comment posted", nil)
}

func (s *Server) postPRClose(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo
	err = ghpr.ClosePRWith(ctx.Context(), s.ghRun(), cwd, owner, repo, n)
	s.auditAction(ctx, audit.ActionPRClose, err, map[string]any{
		"owner": owner, "repo": repo, "number": n, "project": project,
	})
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	if s.bot != nil {
		s.bot.ApplyPRTerminalState(owner, repo, n, "CLOSED")
	}
	return s.prRedirect(ctx, owner, repo, n, project, "PR closed", nil)
}

func (s *Server) postPRMerge(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	method := ghpr.NormalizeMergeMethod(ctx.PostFormValue("method"))
	if method == "" || ctx.PostFormValue("method") == "" {
		method = ghpr.NormalizeMergeMethod(s.cfg.WebMergeMethodValue())
	}
	attemptAnyway := ctx.PostFormValue("attemptAnyway") == "1" ||
		strings.EqualFold(ctx.PostFormValue("attemptAnyway"), "on")

	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	owner, repo = ref.Owner, ref.Repo
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	detail, viewErr := ghpr.ViewPRDetailWith(ctx.Context(), s.ghRun(), cwd, selector)
	if viewErr != nil {
		s.auditAction(ctx, audit.ActionPRMerge, viewErr, map[string]any{
			"owner": owner, "repo": repo, "number": n, "phase": "view",
		})
		return s.prRedirect(ctx, owner, repo, n, project, "", viewErr)
	}
	pre := ghpr.CheckMergePreflight(detail.State, detail.Mergeable, detail.Checks, attemptAnyway)
	if !pre.Allow {
		err := fmt.Errorf("%s", pre.Reason)
		s.auditAction(ctx, audit.ActionPRMerge, err, map[string]any{
			"owner": owner, "repo": repo, "number": n, "phase": "preflight",
		})
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	err = ghpr.MergePRWith(ctx.Context(), s.ghRun(), cwd, owner, repo, n, ghpr.MergeOpts{
		Method:        method,
		AttemptAnyway: attemptAnyway,
	})
	detailMap := map[string]any{
		"owner": owner, "repo": repo, "number": n, "method": string(method),
		"attemptAnyway": attemptAnyway, "project": project,
	}
	if err != nil {
		s.auditAction(ctx, audit.ActionPRMerge, err, detailMap)
		return s.prRedirect(ctx, owner, repo, n, project, "", err)
	}
	var threads []string
	if s.bot != nil {
		threads = s.bot.ApplyPRTerminalState(owner, repo, n, "MERGED")
		detailMap["threads"] = threads
	}
	s.auditAction(ctx, audit.ActionPRMerge, nil, detailMap)
	msg := "PR merged (" + string(method) + ")"
	if len(threads) > 0 {
		msg += fmt.Sprintf("; updated %d session(s)", len(threads))
	}
	return s.prRedirect(ctx, owner, repo, n, project, msg, nil)
}

func (s *Server) issueRedirect(ctx *hime.Context, project, owner, repo string, n int, okMsg string, err error) error {
	q := url.Values{}
	q.Set("owner", owner)
	q.Set("repo", repo)
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	return ctx.Redirect(fmt.Sprintf("/projects/%s/issues/%d?%s", project, n, q.Encode()))
}

func (s *Server) prRedirect(ctx *hime.Context, owner, repo string, n int, project, okMsg string, err error) error {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	u := fmt.Sprintf("/prs/%s/%s/%d", owner, repo, n)
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}
