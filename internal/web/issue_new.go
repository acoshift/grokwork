package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// issueNewPage renders the GitHub issue intake form. The page is readable without
// the githubWrites capability; the hard gate lives on the POST (requireFeature +
// requireMember + per-project GithubWrites), matching case_new / postPRGitHubReview.
func (s *Server) issueNewPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	if _, err := s.projectPath(project); err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)

	d := s.basePage(ctx)
	d.Title = project + " · New issue"
	d.IsIssues = true
	d.Project = project
	d.RepoCatalog = catalog
	d.CanCreateIssue = s.canCreateIssue(d, project)
	d.IssueKind = normalizeIssueKind(ctx.FormValue("kind"))
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	} else if pickErr != nil {
		// Empty catalog / bad picker still renders the page; the form is inert
		// without a repo pair to post.
		d.Error = pickErr.Error()
	}
	if pickErr == nil {
		d.ActiveOwner = active.Owner
		d.ActiveRepo = active.Repo
	}
	return s.viewPage(ctx, "issue_new", d)
}

// canCreateIssue is the UI gate for the new-issue form and list CTA: deployment
// githubWrites feature + member role (CanGitHubWrite) and the per-project
// GithubWrites capability. Hiding the form is not a gate — POST re-checks.
func (s *Server) canCreateIssue(d pageData, project string) bool {
	if !d.CanGitHubWrite {
		return false
	}
	return s.cfg.ResolveCapabilities(project, d.UserID).GithubWrites
}

// postIssueNew creates a GitHub feature/bug issue via the host gh credential.
func (s *Server) postIssueNew(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}

	kind := strings.TrimSpace(ctx.PostFormValue("kind"))
	if kind != "feature" && kind != "bug" {
		return s.issueNewRedirect(ctx, project,
			strings.TrimSpace(ctx.PostFormValue("owner")),
			strings.TrimSpace(ctx.PostFormValue("repo")),
			kind,
			"kind must be feature or bug")
	}
	owner, repo := pickedRepo(
		ctx.PostFormValue("repo_full"),
		ctx.PostFormValue("owner"),
		ctx.PostFormValue("repo"),
	)
	title := strings.TrimSpace(ctx.PostFormValue("title"))
	if title == "" {
		return s.issueNewRedirect(ctx, project, owner, repo, kind, "title is required")
	}

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.issueNewRedirect(ctx, project, owner, repo, kind, err.Error())
	}
	owner, repo = ref.Owner, ref.Repo

	detailMap := map[string]any{
		"project": project, "owner": owner, "repo": repo, "kind": kind,
	}

	// Route gate is deployment-wide; this one is per project. Hiding the form is
	// not enough — same pattern as postPRGitHubReview.
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).GithubWrites {
		denied := errors.New("not allowed to create GitHub issues for this project")
		s.auditAction(ctx, audit.ActionIssueCreate, denied, detailMap)
		return ctx.Status(http.StatusForbidden).Error("forbidden: " + denied.Error())
	}

	// Host gh files the issue; body is the only place the human is named.
	body := s.attributeCommentBody(ctx, ctx.PostFormValue("body"))

	n, _, createErr := ghpr.CreateIssueWith(ctx.Context(), s.ghRun(), cwd, owner, repo, ghpr.CreateIssueOpts{
		Title:  title,
		Body:   body,
		Labels: []string{kind},
	})
	detailMap["number"] = n
	s.auditAction(ctx, audit.ActionIssueCreate, createErr, detailMap)
	if createErr != nil {
		return s.issueNewRedirect(ctx, project, owner, repo, kind, userFacingErr(createErr))
	}
	s.invalidateIssueListCache(project, owner, repo)
	return s.issueRedirect(ctx, project, owner, repo, n, "Issue created", nil)
}

// issueNewRedirect sends the operator back to the form with context preserved.
// Title and body are deliberately not re-echoed (they can carry customer data).
func (s *Server) issueNewRedirect(ctx *hime.Context, project, owner, repo, kind, errMsg string) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if kind == "feature" || kind == "bug" {
		q.Set("kind", kind)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	loc := "/projects/" + url.PathEscape(project) + "/issues/new"
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}

func normalizeIssueKind(kind string) string {
	if strings.TrimSpace(kind) == "bug" {
		return "bug"
	}
	return "feature"
}
