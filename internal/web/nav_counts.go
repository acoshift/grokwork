package web

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/ghpr"
)

const (
	navCountTimeout      = 4 * time.Second
	errorCountTTL        = 20 * time.Second
	errorCountMaxEntries = 32
)

type errorCountCacheEntry struct {
	n  int
	at time.Time
}

// navCounts is the JSON body for GET /partials/nav/counts.
// Issues and errors are workspace-only; omitzero keeps them off the global shell.
type navCounts struct {
	Ship    int `json:"ship"`
	Cases   int `json:"cases"`
	Reviews int `json:"reviews"`
	Issues  int `json:"issues,omitzero"`
	Errors  int `json:"errors,omitzero"`
}

// partialNavCounts returns active-item counts for the sidebar badges.
// Ship / cases / reviews are local store reads. Issues and errors hit
// GitHub / error sources and are skipped on the global shell.
func (s *Server) partialNavCounts(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	if project != "" {
		if err := s.ensureProjectAccess(ctx, project); err != nil {
			return forbiddenProject(ctx, err)
		}
	}

	out := navCounts{
		Ship:    s.listShipBoardVisible(ctx, project, "open").Open,
		Cases:   s.listCaseBoardVisible(ctx, project).OpenTotal,
		Reviews: s.pendingReviewCount(ctx, project),
	}
	// Remote counts (GitHub issues, error sources) are a second fetch so the
	// local badges are not held behind those APIs. JS requests remote=1 when
	// the nav actually has Issues/Errors placeholders.
	if project == "" || ctx.FormValue("remote") != "1" {
		return ctx.NoCache().JSON(out)
	}

	rctx, cancel := context.WithTimeout(ctx.Context(), navCountTimeout)
	defer cancel()

	var issues, errors int
	var wg sync.WaitGroup
	wg.Go(func() { issues = s.countActiveIssues(rctx, project) })
	wg.Go(func() { errors = s.countActiveErrors(rctx, project) })
	wg.Wait()
	out.Issues = issues
	out.Errors = errors
	return ctx.NoCache().JSON(out)
}

func (s *Server) countActiveIssues(ctx context.Context, project string) int {
	path, err := s.projectPath(project)
	if err != nil {
		return 0
	}
	catalog, err := s.cfg.ProjectRepoCatalogWith(ctx, project, nil)
	if err != nil || len(catalog) == 0 {
		return 0
	}
	active, err := config.ResolveRepoPicker(catalog, "", "")
	if err != nil || !active.Valid() {
		return 0
	}
	cacheKey := project + "\x00" + active.Owner + "\x00" + active.Repo + "\x00" + "open"
	issues, err := s.cachedListIssues(ctx, cacheKey, path, ghpr.IssueListOpts{
		Owner: active.Owner,
		Repo:  active.Repo,
		State: "open",
		Limit: issueListLimit,
	})
	if err != nil {
		return 0
	}
	return len(issues)
}

func (s *Server) countActiveErrors(ctx context.Context, project string) int {
	if s.cfg == nil || !s.cfg.ProjectErrorsAnyEnabled(project) {
		return 0
	}
	if n, ok := s.cachedErrorCount(project); ok {
		return n
	}
	var (
		n      int
		failed bool
		mu     sync.Mutex
		wg     sync.WaitGroup
	)
	add := func(c int, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = true
			return
		}
		n += c
	}
	if s.cfg.ProjectDeploysErrorsCanResolve(project) {
		wg.Go(func() { add(s.countDeploysErrors(ctx, project)) })
	}
	if s.cfg.ProjectSentryCanResolve(project) {
		wg.Go(func() { add(s.countSentryErrors(ctx, project)) })
	}
	if s.cfg.ProjectGCPErrorsCanResolve(project) {
		wg.Go(func() { add(s.countGCPErrors(ctx, project)) })
	}
	wg.Wait()
	if !failed {
		s.storeErrorCount(project, n)
	}
	return n
}

func (s *Server) countDeploysErrors(ctx context.Context, project string) (int, error) {
	cfg := s.cfg.ProjectDeploysErrors(project)
	if cfg == nil || strings.TrimSpace(cfg.Project) == "" {
		return 0, nil
	}
	res, err := s.deploysClient(project).List(ctx, deploys.ListReq{
		Project: cfg.Project,
		Limit:   errorListCap,
	})
	if err != nil {
		return 0, err
	}
	return len(res.Groups), nil
}

func (s *Server) countSentryErrors(ctx context.Context, project string) (int, error) {
	res, err := s.sentryClient(project).List(ctx, errsrc.ListQuery{Limit: errorListCap})
	if err != nil {
		return 0, err
	}
	return len(res.Groups), nil
}

func (s *Server) countGCPErrors(ctx context.Context, project string) (int, error) {
	res, err := s.gcpClient(project).List(ctx, errsrc.ListQuery{Limit: errorListCap})
	if err != nil {
		return 0, err
	}
	return len(res.Groups), nil
}

func (s *Server) cachedErrorCount(project string) (int, bool) {
	now := time.Now()
	s.errorCountMu.Lock()
	defer s.errorCountMu.Unlock()
	e, ok := s.errorCounts[project]
	if !ok || now.Sub(e.at) >= errorCountTTL {
		return 0, false
	}
	return e.n, true
}

func (s *Server) storeErrorCount(project string, n int) {
	now := time.Now()
	s.errorCountMu.Lock()
	defer s.errorCountMu.Unlock()
	if s.errorCounts == nil {
		s.errorCounts = map[string]errorCountCacheEntry{}
	}
	for k, e := range s.errorCounts {
		if now.Sub(e.at) >= errorCountTTL {
			delete(s.errorCounts, k)
		}
	}
	if len(s.errorCounts) >= errorCountMaxEntries {
		oldest, oldestAt := "", now
		for k, e := range s.errorCounts {
			if e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.errorCounts, oldest)
	}
	s.errorCounts[project] = errorCountCacheEntry{n: n, at: now}
}
