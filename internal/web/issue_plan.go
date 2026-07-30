package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/ghpr"
)

// postIssuePlan starts an agent session that writes a GitHub tasklist onto the
// feature issue (Phase 2 "Plan this feature").
func (s *Server) postIssuePlan(ctx *hime.Context) error {
	if !s.cfg.FeatureStartSessions() {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkStartRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "kind": "feature_plan", "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	// Best-effort title/body for the prompt; a fetch failure still dispatches
	// with number/URL only (same stance as Fix).
	info, _ := ghpr.ViewIssueWith(ctx.Context(), s.ghRun(), cwd, n, owner, repo)
	title := strings.TrimSpace(info.Title)
	body := info.Body
	issueURL := strings.TrimSpace(info.URL)
	if issueURL == "" {
		issueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
	}

	actor := s.fixActor(ctx)
	model := strings.TrimSpace(ctx.PostFormValue("model"))
	res, startErr := s.bot.StartFeaturePlan(bot.FeaturePlanOpts{
		Project: project,
		Actor:   actor,
		Owner:   owner,
		Repo:    repo,
		Number:  n,
		Title:   title,
		URL:     issueURL,
		Body:    body,
		Model:   model,
	})

	detailMap := map[string]any{
		"project": project, "kind": "feature_plan",
		"owner": owner, "repo": repo, "number": n, "model": model,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detailMap)
		return s.mapIssuePlanError(ctx, project, owner, repo, n, startErr)
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detailMap)
	return s.sessionRedirect(ctx, res.ThreadID, fixStatusFlash(res.Status), "")
}

// postIssueItemStart starts a session for one tasklist line of a feature issue.
func (s *Server) postIssueItemStart(ctx *hime.Context) error {
	if !s.cfg.FeatureStartSessions() {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	rawLine := ctx.PostFormValue("raw_line") // exact line key; do not trim interior

	project, ref, cwd, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	if strings.TrimSpace(rawLine) == "" {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "",
			fmt.Errorf("missing checklist line"))
	}

	if err := s.checkStartRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "kind": "checklist_item", "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	info, viewErr := ghpr.ViewIssueWith(ctx.Context(), s.ghRun(), cwd, n, owner, repo)
	if viewErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, viewErr, map[string]any{
			"project": project, "kind": "checklist_item", "owner": owner, "repo": repo, "number": n,
		})
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "", viewErr)
	}

	// Re-parse against the live body: refuse when the line is gone, already
	// started, or already checked (body changed since the page rendered).
	items := bot.ParseTasklist(info.Body)
	var hit *bot.TasklistItem
	wantRaw := strings.TrimRight(rawLine, "\r\n")
	for i := range items {
		if items[i].RawLine == wantRaw {
			hit = &items[i]
			break
		}
	}
	if hit == nil {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "",
			fmt.Errorf("checklist item not found — the issue body may have changed; reload and try again"))
	}
	if hit.SessionURL != "" {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "",
			fmt.Errorf("this checklist item already has a session"))
	}
	if hit.Checked {
		return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "",
			fmt.Errorf("this checklist item is already checked"))
	}

	issueURL := strings.TrimSpace(info.URL)
	if issueURL == "" {
		issueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
	}

	actor := s.fixActor(ctx)
	model := strings.TrimSpace(ctx.PostFormValue("model"))
	res, startErr := s.bot.StartChecklistItem(bot.ChecklistItemOpts{
		Project:    project,
		Owner:      owner,
		Repo:       repo,
		Number:     n,
		IssueTitle: strings.TrimSpace(info.Title),
		IssueURL:   issueURL,
		ItemText:   hit.Text,
		RawLine:    hit.RawLine,
		Actor:      actor,
		Model:      model,
	})

	detailMap := map[string]any{
		"project": project, "kind": "checklist_item",
		"owner": owner, "repo": repo, "number": n, "model": model,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detailMap)
		return s.mapIssuePlanError(ctx, project, owner, repo, n, startErr)
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detailMap)
	return s.sessionRedirect(ctx, res.ThreadID, fixStatusFlash(res.Status), "")
}

func (s *Server) mapIssuePlanError(ctx *hime.Context, project, owner, repo string, n int, err error) error {
	return s.issuePlanSourceRedirect(ctx, project, owner, repo, n, "", err)
}

func (s *Server) issuePlanSourceRedirect(ctx *hime.Context, project, owner, repo string, n int, _ string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if err != nil {
		q.Set("err", err.Error())
	}
	loc := fmt.Sprintf("/projects/%s/issues/%d", url.PathEscape(project), n)
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}
