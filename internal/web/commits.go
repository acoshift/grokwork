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
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

func (s *Server) commitsList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, err := config.ResolveRepoPicker(catalog, owner, repo)
	if err != nil {
		d := s.basePage(ctx)
		d.Title = "Commits · " + project
		d.IsCommits = true
		d.Project = project
		d.RepoCatalog = catalog
		d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
		if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
			d.Error = e
		} else {
			d.Error = err.Error()
		}
		return s.viewPage(ctx, "commits", d)
	}
	ref := strings.TrimSpace(ctx.FormValue("ref"))
	if ref == "" {
		ref = "HEAD"
	}
	pageSize := ghpr.DefaultCommitListLimit
	page := 1
	if p, err := strconv.Atoi(strings.TrimSpace(ctx.FormValue("page"))); err == nil && p > 0 {
		page = p
	}
	skip := (page - 1) * pageSize
	d := s.basePage(ctx)
	d.Title = "Commits · " + project
	d.IsCommits = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.CommitRef = ref
	d.CommitPage = page
	d.CommitHasPrev = page > 1
	d.CanReviewCommit = d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		if d.Error == "" {
			d.Error = pathErr.Error()
		}
		return s.viewPage(ctx, "commits", d)
	}
	// Fetch one extra row so we know whether a next page exists.
	list, listErr := ghpr.ListCommitsWith(ctx.Context(), s.ghRun(), repoPath, ghpr.CommitListOpts{
		Ref:   ref,
		Limit: pageSize + 1,
		Skip:  skip,
	})
	if len(list) > pageSize {
		d.CommitHasNext = true
		list = list[:pageSize]
	}
	d.Commits = list
	if d.Error == "" && listErr != nil {
		d.Error = listErr.Error()
	}
	return s.viewPage(ctx, "commits", d)
}

// postCommitsFetch runs git fetch --all --prune on the selected repo checkout
// so the commits browser can show up-to-date remote-tracking refs. Shallow
// clones are unshallowed so full history is available for listing and review.
func (s *Server) postCommitsFetch(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	root, err := s.projectPath(project)
	if err != nil {
		return s.commitsListRedirect(ctx, project, owner, repo, ref, "", err)
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pickErr != nil {
		return s.commitsListRedirect(ctx, project, owner, repo, ref, "", pickErr)
	}
	path, err := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if err != nil {
		return s.commitsListRedirect(ctx, project, active.Owner, active.Repo, ref, "", err)
	}
	err = ghpr.FetchWith(ctx.Context(), s.ghRun(), path)
	s.auditAction(ctx, audit.ActionGitFetch, err, map[string]any{
		"project": project, "owner": active.Owner, "repo": active.Repo,
	})
	if err != nil {
		return s.commitsListRedirect(ctx, project, active.Owner, active.Repo, ref, "", err)
	}
	gitworktree.NoteFetched(path)
	return s.commitsListRedirect(ctx, project, active.Owner, active.Repo, ref, "Fetched remotes (full history)", nil)
}

func (s *Server) commitsListRedirect(ctx *hime.Context, project, owner, repo, ref, okMsg string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if ref != "" {
		q.Set("ref", ref)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	u := fmt.Sprintf("/projects/%s/commits", url.PathEscape(project))
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func (s *Server) commitDetail(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	sha := strings.TrimSpace(ctx.PathValue("sha"))
	if sha == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing commit sha")
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	var active config.GitHubRepoRef
	var path string
	if owner != "" || repo != "" {
		_, active, path, err = s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
		if err != nil {
			return ctx.Status(http.StatusForbidden).Error(err.Error())
		}
	} else if len(catalog) > 0 {
		active = catalog[0]
		path, err = gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
		if err != nil {
			d := s.basePage(ctx)
			d.Title = "commit · " + project
			d.IsCommits = true
			d.Project = project
			d.RepoCatalog = catalog
			d.ActiveOwner = active.Owner
			d.ActiveRepo = active.Repo
			d.Error = err.Error()
			return s.viewPage(ctx, "commit_detail", d)
		}
	} else {
		path, err = gitworktree.ResolveLocalRepo(ctx.Context(), root, "", "")
		if err != nil {
			return ctx.Status(http.StatusBadRequest).Error(err.Error())
		}
	}
	detail, showErr := ghpr.ShowCommitMetaWith(ctx.Context(), s.ghRun(), path, sha)
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("%s · %s", shortOr(detail.ShortSHA, sha), project)
	d.IsCommits = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.Commit = detail
	if showErr == nil && detail.SHA != "" {
		index, idxErr := ghpr.CommitDiffIndexWith(ctx.Context(), s.ghRun(), path, detail.SHA)
		fragBase := fmt.Sprintf("/projects/%s/commits/%s/file", url.PathEscape(project), url.PathEscape(detail.SHA))
		qExtra := url.Values{}
		if active.Owner != "" {
			qExtra.Set("owner", active.Owner)
		}
		if active.Repo != "" {
			qExtra.Set("repo", active.Repo)
		}
		d.DiffReview = buildDiffReview(index, "c:"+detail.SHA, func(f ghpr.FileStat) string {
			return fragBase + "?" + fragQuery(f, qExtra)
		})
		if idxErr != nil {
			showErr = idxErr
		}
	}
	d.CanReviewCommit = d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if showErr != nil {
		d.Error = showErr.Error()
	}
	return s.viewPage(ctx, "commit_detail", d)
}

// postCommitReview starts a new Discord/web session that agentically reviews the
// commit and opens GitHub issues (Grok owns gh issue create; bot does not file).
func (s *Server) postCommitReview(ctx *hime.Context) error {
	if !s.cfg.FeatureStartSessions() {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	project := strings.TrimSpace(ctx.PathValue("project"))
	sha := strings.TrimSpace(ctx.PathValue("sha"))
	if sha == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing commit sha")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.commitReviewSourceRedirect(ctx, project, sha, owner, repo, err)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, err, map[string]any{
			"project": project, "owner": owner, "repo": repo, "sha": sha,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	detail, showErr := ghpr.ShowCommitMetaWith(ctx.Context(), s.ghRun(), cwd, sha)
	if showErr != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, showErr, map[string]any{
			"project": project, "owner": owner, "repo": repo, "sha": sha,
		})
		return s.commitReviewSourceRedirect(ctx, project, sha, owner, repo, showErr)
	}

	actor := s.fixActor(ctx)
	author := strings.TrimSpace(detail.AuthorName)
	if detail.AuthorEmail != "" {
		if author != "" {
			author += " <" + detail.AuthorEmail + ">"
		} else {
			author = detail.AuthorEmail
		}
	}
	date := ""
	if !detail.AuthorDate.IsZero() {
		date = detail.AuthorDate.UTC().Format("2006-01-02 15:04 UTC")
	}

	res, startErr := s.bot.StartCommitReview(bot.CommitReviewOpts{
		Project:  project,
		Actor:    actor,
		Owner:    owner,
		Repo:     repo,
		SHA:      detail.SHA,
		ShortSHA: detail.ShortSHA,
		Subject:  detail.Subject,
		Body:     detail.Body,
		Author:   author,
		Date:     date,
	})

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo, "sha": detail.SHA,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, startErr, detailMap)
		return s.mapCommitReviewError(ctx, project, detail.SHA, owner, repo, startErr)
	}
	s.auditAction(ctx, audit.ActionCommitReviewStart, nil, detailMap)

	ok := string(res.Status)
	if res.DiscordOffline {
		ok = ok + "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) mapCommitReviewError(ctx *hime.Context, project, sha, owner, repo string, err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, bot.ErrDiscordNotReady):
		return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusServiceUnavailable)
	case errors.Is(err, bot.ErrQueueFull):
		return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusConflict)
	case errors.Is(err, bot.ErrProjectRequired):
		return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusBadRequest)
	default:
		low := strings.ToLower(msg)
		if strings.Contains(low, "channel") || strings.Contains(low, "mapped") {
			return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusBadRequest)
		}
		return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusBadRequest)
	}
}

func (s *Server) commitReviewSourceRedirect(ctx *hime.Context, project, sha, owner, repo string, err error) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, http.StatusFound)
}

func (s *Server) commitReviewSourceRedirectStatus(ctx *hime.Context, project, sha, owner, repo, errMsg string, status int) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	u := fmt.Sprintf("/projects/%s/commits/%s", url.PathEscape(project), url.PathEscape(sha))
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// Match fixSourceRedirect: 400 → browser flash redirect; 409/503 keep status for tests.
	switch status {
	case http.StatusFound, http.StatusSeeOther, 0, http.StatusBadRequest:
		return ctx.Redirect(u)
	case http.StatusTooManyRequests, http.StatusConflict, http.StatusServiceUnavailable, http.StatusForbidden:
		return ctx.Status(status).Error(errMsg)
	default:
		return ctx.Redirect(u)
	}
}

func shortOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
