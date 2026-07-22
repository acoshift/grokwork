package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
)

// sessionIdentity returns Discord user id and web role for project ACL.
// When web auth is disabled (open LAN), returns admin with empty user id.
func (s *Server) sessionIdentity(ctx *hime.Context) (userID string, role config.WebRole) {
	if s == nil || s.cfg == nil || !s.cfg.WebAuthEnabled() {
		return "", config.WebRoleAdmin
	}
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return "", config.WebRoleNone
	}
	return sess.DiscordUserID, sess.Role
}

// ensureProjectAccess returns nil if the caller may open the project.
func (s *Server) ensureProjectAccess(ctx *hime.Context, project string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if _, ok := s.cfg.ProjectPath(project); !ok {
		return fmt.Errorf("unknown project %q", project)
	}
	userID, role := s.sessionIdentity(ctx)
	if !s.cfg.CanAccessProject(project, userID, role) {
		return fmt.Errorf("forbidden: no access to project %q", project)
	}
	return nil
}

// threadProject resolves the project for a Discord/web thread (session, then history).
func (s *Server) threadProject(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	if s.sessions != nil {
		if e, ok := s.sessions.Get(threadID); ok {
			if p := strings.TrimSpace(e.Project); p != "" {
				return p
			}
		}
	}
	if s.history != nil {
		if th, err := s.history.Get(threadID); err == nil {
			return strings.TrimSpace(th.Project)
		}
	}
	return ""
}

// ensureThreadAccess checks the caller may open a thread's project.
// Threads with no project are admin-only (matches filterThreadsVisible).
func (s *Server) ensureThreadAccess(ctx *hime.Context, threadID string) (project string, err error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("thread id is required")
	}
	project = s.threadProject(threadID)
	_, role := s.sessionIdentity(ctx)
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		return project, nil
	}
	if project == "" {
		return "", fmt.Errorf("forbidden: no access to thread")
	}
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return "", err
	}
	return project, nil
}

// resolveCatalogRepoAccess is resolveCatalogRepo plus web project ACL.
// When project is empty, only catalogs of projects the session may see are searched
// (so a hidden project cannot steal the match for a shared owner/repo).
func (s *Server) resolveCatalogRepoAccess(ctx *hime.Context, project, owner, repo string) (proj string, ref config.GitHubRepoRef, cwd string, err error) {
	project = strings.TrimSpace(project)
	if project != "" {
		if err := s.ensureProjectAccess(ctx, project); err != nil {
			return "", config.GitHubRepoRef{}, "", err
		}
		return s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	}
	for _, name := range s.filterProjectNames(ctx) {
		p, r, c, rErr := s.resolveCatalogRepo(ctx.Context(), name, owner, repo)
		if rErr == nil {
			return p, r, c, nil
		}
	}
	return "", config.GitHubRepoRef{}, "", fmt.Errorf("repository %s/%s is not in any accessible project catalog",
		strings.TrimSpace(owner), strings.TrimSpace(repo))
}

// filterSnapshotToVisible limits Snapshot projects to what the session may see.
func (s *Server) filterSnapshotToVisible(ctx *hime.Context, snap config.Snapshot) config.Snapshot {
	userID, role := s.sessionIdentity(ctx)
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		return snap
	}
	visible := s.cfg.ProjectsVisibleTo(userID, role)
	set := make(map[string]struct{}, len(visible))
	for _, n := range visible {
		set[n] = struct{}{}
	}
	projects := make([]config.ProjectItem, 0, len(visible))
	for _, p := range snap.Projects {
		if _, ok := set[p.Name]; ok {
			projects = append(projects, p)
		}
	}
	snap.Projects = projects
	snap.ProjectNames = visible
	channels := make([]config.ChannelItem, 0, len(snap.Channels))
	for _, ch := range snap.Channels {
		if _, ok := set[ch.Project]; ok {
			channels = append(channels, ch)
		}
	}
	snap.Channels = channels
	return snap
}

// filterProjectNames returns project names visible to the session.
func (s *Server) filterProjectNames(ctx *hime.Context) []string {
	userID, role := s.sessionIdentity(ctx)
	return s.cfg.ProjectsVisibleTo(userID, role)
}

func (s *Server) filterThreadsVisible(ctx *hime.Context, threads []history.Summary) []history.Summary {
	userID, role := s.sessionIdentity(ctx)
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		return threads
	}
	allowed := make(map[string]struct{})
	for _, n := range s.cfg.ProjectsVisibleTo(userID, role) {
		allowed[n] = struct{}{}
	}
	out := make([]history.Summary, 0, len(threads))
	for _, t := range threads {
		if t.Project == "" {
			continue
		}
		if _, ok := allowed[t.Project]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) filterWorktreesVisible(ctx *hime.Context, list []bot.WorktreeInfo) []bot.WorktreeInfo {
	userID, role := s.sessionIdentity(ctx)
	if config.RoleAtLeast(role, config.WebRoleAdmin) {
		return list
	}
	allowed := make(map[string]struct{})
	for _, n := range s.cfg.ProjectsVisibleTo(userID, role) {
		allowed[n] = struct{}{}
	}
	out := make([]bot.WorktreeInfo, 0, len(list))
	for _, w := range list {
		if _, ok := allowed[w.Project]; ok {
			out = append(out, w)
		}
	}
	return out
}

// forbiddenProject writes a 403 for ensureProjectAccess failures.
func forbiddenProject(ctx *hime.Context, err error) error {
	return ctx.Status(http.StatusForbidden).Error(err.Error())
}
