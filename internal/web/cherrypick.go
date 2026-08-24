package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
)

type cherryPickFileView struct {
	Path    string
	Content string
	TooBig  bool
	Binary  bool
}

func (s *Server) loadCherryPickJob(ctx *hime.Context, project, id string) (gitworktree.Job, error) {
	project = strings.TrimSpace(project)
	id = strings.TrimSpace(id)
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return gitworktree.Job{}, err
	}
	j, err := gitworktree.LoadJob(s.cfg.DataDir, id)
	if err != nil || j.Project != project {
		return gitworktree.Job{}, fmt.Errorf("not found")
	}
	return j, nil
}

func (s *Server) requireCherryPickCanShip(ctx *hime.Context, project string) error {
	userID, _ := s.sessionIdentity(ctx)
	if !s.cfg.ResolveCapabilities(project, userID).CanShip() {
		return errors.New("not allowed to cherry-pick for this project")
	}
	return nil
}

func (s *Server) cherryPickConflictPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("id"))
	j, err := s.loadCherryPickJob(ctx, project, id)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	d := s.basePage(ctx)
	d.Title = project + " · cherry-pick conflict"
	d.IsCommits = true
	d.Project = project
	d.CherryPickJob = j
	d.CherryPickFiles = loadConflictFileViews(ctx.Context(), j)
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	userID, _ := s.sessionIdentity(ctx)
	d.CanCherryPick = s.cfg.ResolveCapabilities(project, userID).CanShip()
	return s.viewPage(ctx, "cherrypick_conflict", d)
}

func jobAllowsPath(ctx context.Context, j gitworktree.Job, path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if slices.Contains(j.Files, path) {
		return true
	}
	cur, err := gitworktree.ConflictedFiles(ctx, j.Checkout)
	return err == nil && slices.Contains(cur, path)
}

func loadConflictFileViews(ctx context.Context, j gitworktree.Job) []cherryPickFileView {
	files := j.Files
	if cur, err := gitworktree.ConflictedFiles(ctx, j.Checkout); err == nil && len(cur) > 0 {
		files = cur
	}
	out := make([]cherryPickFileView, 0, len(files))
	for _, p := range files {
		fv := cherryPickFileView{Path: p}
		raw, err := gitworktree.ReadWorkingFile(j.Checkout, p, 512<<10)
		if err != nil && strings.Contains(err.Error(), "too large") {
			fv.TooBig = true
			out = append(out, fv)
			continue
		}
		if err != nil {
			out = append(out, fv)
			continue
		}
		if !utf8.Valid(raw) || strings.Contains(string(raw), "\x00") {
			fv.Binary = true
		} else {
			fv.Content = string(raw)
		}
		out = append(out, fv)
	}
	return out
}

func (s *Server) postCherryPickFile(ctx *hime.Context) error {
	return s.withOpenJob(ctx, func(j gitworktree.Job) error {
		path := strings.TrimSpace(ctx.PostFormValue("path"))
		if !jobAllowsPath(ctx.Context(), j, path) {
			return fmt.Errorf("unknown conflict path")
		}
		content := ctx.PostFormValue("content")
		return gitworktree.WriteWorkingFile(j.Checkout, path, []byte(content))
	})
}

func (s *Server) postCherryPickOurs(ctx *hime.Context) error {
	return s.withOpenJob(ctx, func(j gitworktree.Job) error {
		path := strings.TrimSpace(ctx.PostFormValue("path"))
		if !jobAllowsPath(ctx.Context(), j, path) {
			return fmt.Errorf("unknown conflict path")
		}
		return gitworktree.CheckoutConflictSide(ctx.Context(), j.Checkout, path, "ours")
	})
}

func (s *Server) postCherryPickTheirs(ctx *hime.Context) error {
	return s.withOpenJob(ctx, func(j gitworktree.Job) error {
		path := strings.TrimSpace(ctx.PostFormValue("path"))
		if !jobAllowsPath(ctx.Context(), j, path) {
			return fmt.Errorf("unknown conflict path")
		}
		return gitworktree.CheckoutConflictSide(ctx.Context(), j.Checkout, path, "theirs")
	})
}

func (s *Server) withOpenJob(ctx *hime.Context, fn func(gitworktree.Job) error) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("id"))
	j, err := s.loadCherryPickJob(ctx, project, id)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	if err := s.requireCherryPickCanShip(ctx, project); err != nil {
		s.auditAction(ctx, audit.ActionGitCherryPick, err, map[string]any{"project": project, "job": id})
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	if !j.Open() {
		return s.cherryPickJobRedirect(ctx, j, "", errors.New("cherry-pick is no longer in conflict"))
	}
	if err := fn(j); err != nil {
		return s.cherryPickJobRedirect(ctx, j, "", err)
	}
	return s.cherryPickJobRedirect(ctx, j, "Updated", nil)
}

func (s *Server) postCherryPickContinue(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("id"))
	j, err := s.loadCherryPickJob(ctx, project, id)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	if err := s.requireCherryPickCanShip(ctx, project); err != nil {
		s.auditAction(ctx, audit.ActionGitCherryPickContinue, err, map[string]any{"project": project, "job": id})
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	if !j.Open() {
		return s.cherryPickJobRedirect(ctx, j, "", errors.New("cherry-pick is no longer in conflict"))
	}
	after := j.Remaining
	if len(after) > 0 && strings.TrimSpace(after[0]) == strings.TrimSpace(j.Current) {
		after = after[1:]
	}
	res, err := gitworktree.ContinueCherryPick(ctx.Context(), gitworktree.ContinueOpts{
		Repo:         j.RepoPath,
		Checkout:     j.Checkout,
		Target:       j.Target,
		FromSHA:      j.FromSHA,
		Current:      j.Current,
		AfterCurrent: after,
	})
	detail := map[string]any{"project": j.Project, "job": j.ID, "target": j.Target}
	if cerr, ok := errors.AsType[*gitworktree.ConflictError](err); ok {
		j.Picked = append(j.Picked, res.Picked...)
		j.Current = cerr.SHA
		j.Remaining = cerr.Remaining
		j.Files = cerr.Files
		j.Status = gitworktree.JobStatusConflict
		_ = gitworktree.SaveJob(s.cfg.DataDir, j)
		s.auditAction(ctx, audit.ActionGitCherryPickContinue, nil, detail)
		return s.cherryPickJobRedirect(ctx, j, "", cerr)
	}
	if errors.Is(err, gitworktree.ErrTargetMoved) {
		s.auditAction(ctx, audit.ActionGitCherryPickContinue, err, detail)
		return s.cherryPickJobRedirect(ctx, j, "", err)
	}
	if err != nil {
		j.Status = gitworktree.JobStatusFailed
		j.Error = err.Error()
		_ = gitworktree.SaveJob(s.cfg.DataDir, j)
		s.auditAction(ctx, audit.ActionGitCherryPickContinue, err, detail)
		return s.cherryPickJobRedirect(ctx, j, "", err)
	}
	j.Status = gitworktree.JobStatusPushed
	j.Picked = append(j.Picked, res.Picked...)
	j.Skipped = append(j.Skipped, res.Skipped...)
	_ = gitworktree.SaveJob(s.cfg.DataDir, j)
	s.auditAction(ctx, audit.ActionGitCherryPickContinue, nil, detail)
	ok := fmt.Sprintf("Cherry-picked onto %s (%s → %s).", j.Target, shortSHA(j.FromSHA), shortSHA(res.ToSHA))
	if res.Noop {
		ok = fmt.Sprintf("Nothing to push onto %s. Checkout removed.", j.Target)
	}
	return s.commitsListRedirect(ctx, j.Project, j.Owner, j.Repo, "origin/"+j.Target, ok, nil)
}

func (s *Server) postCherryPickAbort(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("id"))
	j, err := s.loadCherryPickJob(ctx, project, id)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	if err := s.requireCherryPickCanShip(ctx, project); err != nil {
		s.auditAction(ctx, audit.ActionGitCherryPickAbort, err, map[string]any{"project": project, "job": id})
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	abortErr := gitworktree.AbortCherryPick(ctx.Context(), j.RepoPath, j.Checkout)
	j.Status = gitworktree.JobStatusAborted
	if abortErr != nil {
		j.Error = abortErr.Error()
	}
	_ = gitworktree.SaveJob(s.cfg.DataDir, j)
	s.auditAction(ctx, audit.ActionGitCherryPickAbort, abortErr, map[string]any{"project": j.Project, "job": j.ID})
	return s.commitsListRedirect(ctx, j.Project, j.Owner, j.Repo, "origin/"+j.Target, "Cherry-pick aborted. Nothing pushed.", abortErr)
}

func (s *Server) cherryPickJobRedirect(ctx *hime.Context, j gitworktree.Job, ok string, err error) error {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if ok != "" {
		q.Set("ok", ok)
	}
	u := fmt.Sprintf("/projects/%s/cherrypick/%s", url.PathEscape(j.Project), url.PathEscape(j.ID))
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func (s *Server) postCherryPickSuggest(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	id := strings.TrimSpace(ctx.PathValue("id"))
	j, err := s.loadCherryPickJob(ctx, project, id)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error("not found")
	}
	if err := s.requireCherryPickCanShip(ctx, project); err != nil {
		s.auditAction(ctx, audit.ActionGitCherryPickSuggest, err, map[string]any{"project": project, "job": id})
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	if !j.Open() {
		return ctx.Status(http.StatusConflict).Error("cherry-pick is no longer in conflict")
	}
	w := ctx.ResponseWriter()
	stream, err := newSSEStream(w)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error(err.Error())
	}
	fail := func(msg string) error {
		_ = stream.Error(msg)
		_ = stream.Done()
		return nil
	}
	_ = stream.Status("Resolving conflicts…")
	suggest := s.suggestConflict
	if suggest == nil {
		suggest = grokrun.SuggestConflictResolution
	}
	hooks := &grokrun.SuggestStreamHooks{
		OnTextDelta: func(delta string) { _ = stream.TextDelta(delta) },
		OnThought:   func(delta string) { _ = stream.ThoughtDelta(delta) },
		OnActivity:  func(line string) { _ = stream.Activity(line) },
	}
	cli := s.cfg.ResolveAgentCLI("").CLI()
	text, err := suggest(ctx.Context(), cli, j.Checkout, 3*time.Minute, j.Files, j.Target, j.Current, hooks)
	s.auditAction(ctx, audit.ActionGitCherryPickSuggest, err, map[string]any{
		"project": j.Project, "job": j.ID, "files": len(j.Files),
	})
	if err != nil {
		return fail(err.Error())
	}
	msg := "Suggested a resolution — review the files, then Apply & continue"
	_ = stream.Result(map[string]any{"ok": true, "text": text, "message": msg})
	_ = stream.Done()
	return nil
}
