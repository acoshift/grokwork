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
		d.Error = err.Error()
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
	if listErr != nil {
		d.Error = listErr.Error()
	}
	return s.viewPage(ctx, "commits", d)
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
	detail, showErr := ghpr.ShowCommitWith(ctx.Context(), s.ghRun(), path, sha, ghpr.DiffCaps{})
	d := s.basePage(ctx)
	d.Title = fmt.Sprintf("%s · %s", shortOr(detail.ShortSHA, sha), project)
	d.IsCommits = true
	d.Project = project
	d.RepoCatalog = catalog
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	d.Commit = detail
	d.Diff = detail.Diff
	d.CanReviewCommit = d.CanGitHubWrite && d.CanStartSession
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if showErr != nil {
		d.Error = showErr.Error()
	}

	// Attach review job if requested or latest for this SHA.
	store, storeErr := s.reviewStore()
	if storeErr == nil {
		if jobID := strings.TrimSpace(ctx.FormValue("job")); jobID != "" {
			if j, gerr := store.Get(jobID); gerr == nil {
				d.ReviewJob = j
			}
		}
		if d.ReviewJob == nil && detail.SHA != "" {
			if j, _ := store.LatestForSHA(project, active.Owner, active.Repo, detail.SHA); j != nil {
				d.ReviewJob = j
			}
		}
		// Soft age-out of orphaned running jobs after process restart.
		if d.ReviewJob != nil {
			s.maybeFailStaleJob(store, d.ReviewJob)
		}
	}
	return s.viewPage(ctx, "commit_detail", d)
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
	detail, showErr := ghpr.ShowCommitWith(ctx.Context(), s.ghRun(), cwd, sha, ghpr.DiffCaps{MaxPatchBytes: 1, MaxFiles: 1, MaxHunks: 1})
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

	// Detach from request context; bound by Execute timeout.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		commitreview.Execute(bg, commitreview.Deps{
			Store:   store,
			Git:     s.ghRun(),
			GrokBin: s.cfg.GrokBin,
			Model:   s.cfg.Model,
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
