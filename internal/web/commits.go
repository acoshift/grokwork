package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
		d.Title = project + " · Commits"
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
	pageSize := ghpr.DefaultCommitListLimit
	page := 1
	if p, err := strconv.Atoi(strings.TrimSpace(ctx.FormValue("page"))); err == nil && p > 0 {
		page = p
	}
	skip := (page - 1) * pageSize
	d := s.basePage(ctx)
	d.Title = project + " · Commits"
	d.IsCommits = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.CommitPage = page
	d.CommitHasPrev = page > 1
	d.CanReviewCommit = d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		if ref == "" {
			ref = "HEAD"
		}
		d.CommitRef = ref
		if d.Error == "" {
			d.Error = pathErr.Error()
		}
		return s.viewPage(ctx, "commits", d)
	}
	// Empty ref → primary tip (same as new worktree base), not stale local HEAD.
	if ref == "" {
		pref := ""
		if s.cfg != nil {
			pref = s.cfg.ProjectPrimaryBranch(project)
		}
		ref = gitworktree.PrimaryStartRef(ctx.Context(), repoPath, pref)
	}
	d.CommitRef = ref
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
	if n := len(list); n > 0 {
		d.CommitRangeStart = skip + 1
		d.CommitRangeEnd = skip + n
	}
	d.Commits = list
	if d.Error == "" && listErr != nil {
		d.Error = listErr.Error()
	}
	s.attachCherryPick(ctx, &d, project, repoPath)
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
	s.attachCherryPick(ctx, &d, project, path)
	s.attachModelPicker(&d, project, s.cfg.EffectiveReviewModel())
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if showErr != nil {
		d.Error = showErr.Error()
	}
	return s.viewPage(ctx, "commit_detail", d)
}

// postCommitReview starts a new session that agentically reviews the commit and
// opens GitHub issues (the agent owns gh issue create; the bot does not file).
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

	if err := s.checkStartRate(ctx); err != nil {
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

	model := strings.TrimSpace(ctx.PostFormValue("model"))
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
		Model:    model,
	})

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo, "sha": detail.SHA, "model": model,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, startErr, detailMap)
		return s.mapCommitReviewError(ctx, project, detail.SHA, owner, repo, startErr)
	}
	s.auditAction(ctx, audit.ActionCommitReviewStart, nil, detailMap)

	// No DiscordOffline branch: a commit review is always web-native, so there is no
	// Discord destination to have promised and failed to deliver.
	return s.sessionRedirect(ctx, res.ThreadID, string(res.Status), "")
}

func (s *Server) mapCommitReviewError(ctx *hime.Context, project, sha, owner, repo string, err error) error {
	msg := err.Error()
	status := http.StatusBadRequest
	if errors.Is(err, bot.ErrQueueFull) {
		status = http.StatusConflict
	}
	return s.commitReviewSourceRedirectStatus(ctx, project, sha, owner, repo, msg, status)
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

func (s *Server) attachCherryPick(ctx *hime.Context, d *pageData, project, repoPath string) {
	d.CherryPickTargets = s.effectiveCherryPickTargets(ctx, project, repoPath)
	userID, role := s.sessionIdentity(ctx)
	memberOK := !d.AuthEnabled || config.RoleAtLeast(role, config.WebRoleMember)
	d.CanCherryPick = len(d.CherryPickTargets) > 0 && memberOK && s.cfg.ResolveCapabilities(project, userID).CanShip()
	if list, err := gitworktree.ListJobs(s.cfg.DataDir); err == nil {
		for i := range list {
			if list[i].Open() && list[i].Project == project {
				cp := list[i]
				d.OpenCherryPickJob = &cp
				break
			}
		}
	}
}

func (s *Server) postCommitsCherryPick(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	page := strings.TrimSpace(ctx.PostFormValue("page"))
	target := strings.TrimSpace(ctx.PostFormValue("target"))
	shas := ctx.Request.PostForm["sha"]

	detail := map[string]any{
		"project": project, "owner": owner, "repo": repo, "target": target,
	}
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).CanShip() {
		denied := errors.New("not allowed to cherry-pick for this project")
		s.auditAction(ctx, audit.ActionGitCherryPick, denied, detail)
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	root, err := s.projectPath(project)
	if err != nil {
		return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, "", err)
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pickErr != nil {
		return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, "", pickErr)
	}
	path, err := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if err != nil {
		return s.cherryPickRedirect(ctx, project, active.Owner, active.Repo, ref, page, shas, "", err)
	}
	owner, repo = active.Owner, active.Repo
	detail["owner"] = owner
	detail["repo"] = repo

	allowed := s.effectiveCherryPickTargets(ctx, project, path)
	if target == "" || !slices.Contains(allowed, target) {
		denied := errors.New("cherry-pick target is not allowlisted")
		s.auditAction(ctx, audit.ActionGitCherryPick, denied, detail)
		return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, "", denied)
	}

	if open, ok := gitworktree.OpenJobForTarget(s.cfg.DataDir, project, path, target); ok {
		return ctx.Redirect("/projects/" + url.PathEscape(project) + "/cherrypick/" + url.PathEscape(open.ID))
	}

	id := cherryPickNonce()
	checkout := gitworktree.CherryPickCheckoutPath(s.cfg.DataDir, project, id)
	res, err := gitworktree.CherryPick(ctx.Context(), gitworktree.CherryPickOpts{
		Repo:             path,
		Checkout:         checkout,
		Target:           target,
		SHAs:             shas,
		PreferredPrimary: s.cfg.ProjectPrimaryBranch(project),
	})
	if res.Picked != nil {
		detail["picked"] = res.Picked
	}
	if res.Skipped != nil {
		detail["skipped"] = res.Skipped
	}
	if res.FromSHA != "" {
		detail["from"] = shortSHA(res.FromSHA)
	}
	if res.ToSHA != "" {
		detail["to"] = shortSHA(res.ToSHA)
	}
	detail["noop"] = res.Noop
	if cerr, ok := errors.AsType[*gitworktree.ConflictError](err); ok {
		now := time.Now().UTC()
		job := gitworktree.Job{
			ID:        id,
			Project:   project,
			Owner:     owner,
			Repo:      repo,
			RepoPath:  path,
			Checkout:  checkout,
			Target:    target,
			FromSHA:   res.FromSHA,
			Picked:    res.Picked,
			Skipped:   res.Skipped,
			Current:   cerr.SHA,
			Remaining: cerr.Remaining,
			Files:     cerr.Files,
			Status:    gitworktree.JobStatusConflict,
			ActorID:   userID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		detail["job"] = id
		detail["files"] = len(cerr.Files)
		if saveErr := gitworktree.SaveJob(s.cfg.DataDir, job); saveErr != nil {
			_ = gitworktree.AbortCherryPick(ctx.Context(), path, checkout)
			s.auditAction(ctx, audit.ActionGitCherryPick, saveErr, detail)
			return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, "", saveErr)
		}
		s.auditAction(ctx, audit.ActionGitCherryPick, nil, detail)
		return ctx.Redirect("/projects/" + url.PathEscape(project) + "/cherrypick/" + url.PathEscape(id))
	}
	s.auditAction(ctx, audit.ActionGitCherryPick, err, detail)
	if err != nil {
		return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, "", err)
	}
	okMsg := fmt.Sprintf("All selected commits are already on %s. Nothing pushed.", target)
	if !res.Noop {
		okMsg = fmt.Sprintf("Cherry-picked %d commit(s) onto %s (%s → %s).",
			len(res.Picked), target, shortSHA(res.FromSHA), shortSHA(res.ToSHA))
	}
	return s.cherryPickRedirect(ctx, project, owner, repo, ref, page, shas, okMsg, nil)
}

func (s *Server) effectiveCherryPickTargets(ctx *hime.Context, project, repoPath string) []string {
	targets := slices.Clone(s.cfg.ProjectCherryPickTargets(project))
	if repoPath != "" && gitworktree.IsRepo(repoPath) {
		pref := s.cfg.ProjectPrimaryBranch(project)
		if name, _, err := gitworktree.ResolvePrimaryBranch(ctx.Context(), repoPath, pref); err == nil {
			targets = slices.DeleteFunc(targets, func(t string) bool { return t == name })
		}
	}
	return targets
}

func (s *Server) cherryPickRedirect(ctx *hime.Context, project, owner, repo, ref, page string, shas []string, okMsg string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	hexSHAs := make([]string, 0, len(shas))
	for _, sha := range shas {
		if gitworktree.IsHexSHA(sha) {
			hexSHAs = append(hexSHAs, sha)
		}
	}
	var u string
	if len(hexSHAs) == 1 {
		u = fmt.Sprintf("/projects/%s/commits/%s", url.PathEscape(project), url.PathEscape(hexSHAs[0]))
	} else {
		if ref != "" {
			q.Set("ref", ref)
		}
		if page != "" && page != "1" {
			q.Set("page", page)
		}
		u = fmt.Sprintf("/projects/%s/commits", url.PathEscape(project))
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func cherryPickNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cp_" + strings.Repeat("0", 32)
	}
	return "cp_" + hex.EncodeToString(b[:])
}
