package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// memberMutationBodyLimit bounds mutating request bodies on member routes.
// Generous over the 50 MiB attachment total (multipart framing + form fields);
// beyond it the CSRF token cannot be read and the request 403s. Bodies in the
// 50–64 MiB gap parse fine and get SaveWebAttachments' friendlier total error.
const memberMutationBodyLimit = 64 << 20

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
		case "deploy":
			on = s.cfg.FeatureDeploy()
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
			// Cap the body BEFORE checkCSRF: its FormValue fallback is what parses
			// a multipart body (spilling to disk uncapped otherwise), so a limit
			// installed later in a handler arrives after the bytes already landed.
			r.Body = http.MaxBytesReader(w, r.Body, memberMutationBodyLimit)
			if !s.checkCSRF(r, sess) {
				http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	}))
}

// attributeCommentBody prefixes host-bot GitHub comment bodies with
// "On behalf of @login …" when the acting account has a linked GitHub login.
//
// The id from sessionDisplay is already the account (canonical-at-mint), which
// is the only key the link is recorded under. No link → the body goes out
// unchanged: the bot's own comment, with no invented handle attached to it.
func (s *Server) attributeCommentBody(ctx *hime.Context, body string) string {
	id, name := s.sessionDisplay(ctx)
	login, _, _ := s.identity.GitHubFor(id)
	return bot.OnBehalfOfCommentBody(name, login, body)
}

func (s *Server) postIssueComment(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	body := s.attributeCommentBody(ctx, ctx.PostFormValue("body"))
	project, ref, path, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
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
	body = s.attributeCommentBody(ctx, body)
	project, ref, path, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
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
	body := s.attributeCommentBody(ctx, ctx.PostFormValue("body"))
	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
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
	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
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

	// Merge failures open the alert modal (required reviews, conflicts, gh
	// branch protection, …) so the reason is not lost under a page flash.
	mergeFail := func(err error) error {
		return s.prRedirectAlert(ctx, owner, repo, n, project, userFacingErr(err), "Merge failed")
	}

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return mergeFail(err)
	}
	owner, repo = ref.Owner, ref.Repo
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	detail, viewErr := ghpr.ViewPRDetailWith(ctx.Context(), s.ghRun(), cwd, selector)
	if viewErr != nil {
		s.auditAction(ctx, audit.ActionPRMerge, viewErr, map[string]any{
			"owner": owner, "repo": repo, "number": n, "phase": "view",
		})
		return mergeFail(viewErr)
	}
	pre := ghpr.CheckMergePreflight(detail, attemptAnyway)
	if !pre.Allow {
		err := fmt.Errorf("%s", pre.Reason)
		s.auditAction(ctx, audit.ActionPRMerge, err, map[string]any{
			"owner": owner, "repo": repo, "number": n, "phase": "preflight",
		})
		return mergeFail(err)
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
		return mergeFail(err)
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
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	return s.prRedirectTo(ctx, owner, repo, n, project, okMsg, errMsg, "")
}

// prRedirectAlert surfaces a failure to the PR page. For htmx (boosted merge
// form), a 3xx→swap path is easy to lose (XHR redirect follow + select), so we
// return 204 + HX-Trigger and open the modal from the response header instead.
// Non-htmx keeps the classic redirect + flash/query-param path.
func (s *Server) prRedirectAlert(ctx *hime.Context, owner, repo string, n int, project, errMsg, alertTitle string) error {
	if ctx.IsHTMX() && setAppAlertTrigger(ctx, alertTitle, errMsg) == nil {
		// 204: no swap. Layout listens for the app-alert event from HX-Trigger.
		return ctx.NoContent()
	}
	return s.prRedirectTo(ctx, owner, repo, n, project, "", errMsg, alertTitle)
}

func (s *Server) prRedirectTo(ctx *hime.Context, owner, repo string, n int, project, okMsg, errMsg, alertTitle string) error {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
		if alertTitle != "" {
			q.Set("alert", alertTitle)
		}
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	u := fmt.Sprintf("/prs/%s/%s/%d", owner, repo, n)
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

// setAppAlertTrigger writes HX-Trigger so layout.tmpl's app-alert listener
// opens the confirm/alert modal with title + message.
func setAppAlertTrigger(ctx *hime.Context, title, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("empty alert message")
	}
	if strings.TrimSpace(title) == "" {
		title = "Action failed"
	}
	payload, err := json.Marshal(map[string]any{
		"app-alert": map[string]string{
			"title":   title,
			"message": message,
		},
	})
	if err != nil {
		return err
	}
	ctx.SetHeader("HX-Trigger", string(payload))
	return nil
}

// userFacingErr strips the "gh <args>: " prefix execRunner wraps around
// stderr so the modal shows GitHub's reason (required reviews, etc.).
func userFacingErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.HasPrefix(s, "gh ") {
		if i := strings.Index(s, ": "); i >= 0 {
			if rest := strings.TrimSpace(s[i+2:]); rest != "" {
				return rest
			}
		}
	}
	return s
}
