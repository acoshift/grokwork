package web

import (
	"context"
	"strings"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

// ResolveProjectRepo is the cookie-free catalog picker for API + web issue create.
func (s *Server) ResolveProjectRepo(ctx context.Context, project, owner, repo, actorID string) (config.GitHubRepoRef, string, error) {
	project = strings.TrimSpace(project)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if project == "" || !s.cfg.AccessAllowed(project, actorID) {
		return config.GitHubRepoRef{}, "", errAPI(404, "not_found", "not found")
	}
	root, err := s.projectPath(project)
	if err != nil {
		return config.GitHubRepoRef{}, "", errAPI(400, "validation", err.Error())
	}
	cat, err := s.cfg.ProjectRepoCatalogWith(ctx, project, nil)
	if err != nil {
		return config.GitHubRepoRef{}, "", errAPI(400, "validation", err.Error())
	}
	if owner == "" && repo == "" && len(cat) > 1 {
		return config.GitHubRepoRef{}, "", errAPI(400, "validation", "owner and repo are required")
	}
	ref, err := config.ResolveRepoPicker(cat, owner, repo)
	if err != nil {
		return config.GitHubRepoRef{}, "", errAPI(400, "validation", err.Error())
	}
	if local, lErr := gitworktree.ResolveLocalRepo(ctx, root, ref.Owner, ref.Repo); lErr == nil {
		return ref, local, nil
	}
	return ref, root, nil
}
