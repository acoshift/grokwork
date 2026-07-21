package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/commitreview"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
)

func (s *Server) reviewStore() (*commitreview.Store, error) {
	s.reviewsOnce.Do(func() {
		s.reviews, s.reviewsErr = commitreview.NewStore(s.cfg.DataDir)
	})
	return s.reviews, s.reviewsErr
}

func (s *Server) commitsList(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	path, err := s.projectPath(project)
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
	limit := 50
	if n, err := strconv.Atoi(strings.TrimSpace(ctx.FormValue("n"))); err == nil && n > 0 {
		limit = n
	}
	list, listErr := ghpr.ListCommitsWith(ctx.Context(), s.ghRun(), path, ghpr.CommitListOpts{
		Ref:   ref,
		Limit: limit,
	})
	d := s.basePage(ctx)
	d.Title = "Commits · " + project
	d.IsCommits = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.CommitRef = ref
	d.Commits = list
	d.CanReviewCommit = d.CanGitHubWrite && d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if listErr != nil {
		d.Error = listErr.Error()
	}
	return s.viewPage(ctx, "commits", d)
}

// postCommitsFetch runs git fetch --all --prune on the project's main checkout
// so the commits browser can show up-to-date remote-tracking refs.
func (s *Server) postCommitsFetch(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	n := strings.TrimSpace(ctx.PostFormValue("n"))
	path, err := s.projectPath(project)
	if err != nil {
		return s.commitsListRedirect(ctx, project, owner, repo, ref, n, "", err)
	}
	err = ghpr.FetchWith(ctx.Context(), s.ghRun(), path)
	s.auditAction(ctx, audit.ActionGitFetch, err, map[string]any{"project": project})
	if err != nil {
		return s.commitsListRedirect(ctx, project, owner, repo, ref, n, "", err)
	}
	return s.commitsListRedirect(ctx, project, owner, repo, ref, n, "Fetched remotes", nil)
}

func (s *Server) commitsListRedirect(ctx *hime.Context, project, owner, repo, ref, n, okMsg string, err error) error {
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
	if n != "" {
		q.Set("n", n)
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
	path, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	_, active, _, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		if owner == "" && repo == "" && len(catalog) > 0 {
			active = catalog[0]
		} else {
			return ctx.Status(http.StatusForbidden).Error(err.Error())
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
		d.DiffReview = buildDiffReview(index, "c:"+detail.SHA, func(f ghpr.FileStat) string {
			return fragBase + "?" + fragQuery(f, nil)
		})
		if idxErr != nil {
			showErr = idxErr
		}
	}
	d.CanReviewCommit = d.CanGitHubWrite && d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if showErr != nil {
		d.Error = showErr.Error()
	}

	// Attach review job if requested or latest for this SHA.
	d.ReviewJob = s.loadReviewJob(project, active.Owner, active.Repo, detail.SHA, ctx.FormValue("job"))
	return s.viewPage(ctx, "commit_detail", d)
}

// commitReviewStatus is a lightweight htmx poll target for the review job card.
// Avoids re-running git show / diff index on every tick while a review runs.
func (s *Server) commitReviewStatus(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	sha := strings.TrimSpace(ctx.PathValue("sha"))
	if sha == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing commit sha")
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	_, active, _, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
		if owner == "" && repo == "" && len(catalog) > 0 {
			active = catalog[0]
		} else {
			return ctx.Status(http.StatusForbidden).Error(err.Error())
		}
	}
	d := s.basePage(ctx)
	d.Project = project
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.ReviewJob = s.loadReviewJob(project, active.Owner, active.Repo, sha, ctx.FormValue("job"))
	if d.ReviewJob == nil {
		return ctx.Status(http.StatusNotFound).Error("review job not found")
	}
	return s.viewFragment(ctx, "commit_detail", "commit_review_job", d)
}

// loadReviewJob returns the requested job, else the latest for this SHA, and
// soft-fails jobs orphaned by a process restart.
func (s *Server) loadReviewJob(project, owner, repo, sha, jobID string) *commitreview.Job {
	store, err := s.reviewStore()
	if err != nil {
		return nil
	}
	var j *commitreview.Job
	if id := strings.TrimSpace(jobID); id != "" {
		if got, gerr := store.Get(id); gerr == nil {
			j = got
		}
	}
	if j == nil && strings.TrimSpace(sha) != "" {
		j, _ = store.LatestForSHA(project, owner, repo, sha)
	}
	if j != nil {
		s.maybeFailStaleJob(store, j)
	}
	return j
}

func (s *Server) maybeFailStaleJob(store *commitreview.Store, j *commitreview.Job) {
	if j == nil {
		return
	}
	switch j.Status {
	case commitreview.StatusQueued, commitreview.StatusRunning, commitreview.StatusCreatingIssues:
	default:
		return
	}
	// In-memory active jobs are tracked; if process restarted, mark old ones failed.
	if time.Since(j.UpdatedAt) > 30*time.Minute {
		j.Status = commitreview.StatusFailed
		j.Error = "review job timed out or server restarted"
		_ = store.Save(j)
	}
}

func (s *Server) postCommitReview(ctx *hime.Context) error {
	// Dual feature gate (middleware only enforces startSessions).
	if !s.cfg.FeatureGitHubWrites() || !s.cfg.FeatureStartSessions() {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	project := strings.TrimSpace(ctx.PathValue("project"))
	sha := strings.TrimSpace(ctx.PathValue("sha"))
	if sha == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing commit sha")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	project, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return s.commitReviewRedirect(ctx, project, sha, owner, repo, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, err, map[string]any{
			"project": project, "owner": owner, "repo": repo, "sha": sha,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	store, err := s.reviewStore()
	if err != nil {
		return s.commitReviewRedirect(ctx, project, sha, owner, repo, "", err)
	}
	if active, _ := store.ActiveForSHA(project, owner, repo, sha); active != nil {
		return s.commitReviewRedirect(ctx, project, sha, owner, repo, active.ID, nil)
	}

	// Resolve SHA / subject for job metadata.
	detail, showErr := ghpr.ShowCommitMetaWith(ctx.Context(), s.ghRun(), cwd, sha)
	if showErr != nil {
		s.auditAction(ctx, audit.ActionCommitReviewStart, showErr, map[string]any{
			"project": project, "owner": owner, "repo": repo, "sha": sha,
		})
		return s.commitReviewRedirect(ctx, project, sha, owner, repo, "", showErr)
	}

	actor := s.fixActor(ctx)
	actorLabel := actor.DisplayName
	if actorLabel == "" {
		actorLabel = actor.ID
	}
	if actorLabel == "" {
		actorLabel = audit.ActorAnonymous
	}
	job := commitreview.NewQueuedJob(commitreview.StartOpts{
		Project:  project,
		Owner:    owner,
		Repo:     repo,
		SHA:      detail.SHA,
		ShortSHA: detail.ShortSHA,
		Subject:  detail.Subject,
		Actor:    actorLabel,
		Cwd:      cwd,
	})
	if err := store.Save(job); err != nil {
		return s.commitReviewRedirect(ctx, project, sha, owner, repo, "", err)
	}

	s.auditAction(ctx, audit.ActionCommitReviewStart, nil, map[string]any{
		"project": project, "owner": owner, "repo": repo, "sha": detail.SHA, "job": job.ID,
	})

	// Detach from request context; bound slightly above Execute's default timeout.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), commitreview.DefaultTimeout+time.Minute)
		defer cancel()
		commitreview.Execute(bg, commitreview.Deps{
			Store:   store,
			Git:     s.ghRun(),
			GrokBin: s.cfg.GrokBin,
			Model:   s.cfg.Model,
			DataDir: s.cfg.DataDir,
		}, job, cwd)
		// Audit issue creates after job finishes.
		if j, err := store.Get(job.ID); err == nil && s.audit != nil {
			for _, f := range j.Findings {
				if f.IssueNumber > 0 {
					_ = s.audit.Append(audit.Event{
						Action: audit.ActionIssueCreate,
						Actor:  actorLabel,
						OK:     true,
						Detail: map[string]any{
							"project": project, "owner": owner, "repo": repo,
							"number": f.IssueNumber, "url": f.IssueURL,
							"sha": detail.SHA, "job": job.ID, "severity": f.Severity,
						},
					})
				}
			}
		}
	}()

	return s.commitReviewRedirect(ctx, project, detail.SHA, owner, repo, job.ID, nil)
}

func (s *Server) commitReviewRedirect(ctx *hime.Context, project, sha, owner, repo, jobID string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if jobID != "" {
		q.Set("job", jobID)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if jobID != "" {
		q.Set("ok", "Review started — filing GitHub issues when done.")
	}
	u := fmt.Sprintf("/projects/%s/commits/%s", url.PathEscape(project), url.PathEscape(sha))
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func shortOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
