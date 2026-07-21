package gitworktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveLocalRepo finds the local git checkout for a GitHub owner/repo under a
// project path. Single-repo projects return projectPath when it is a git root.
// Multi-repo folders (no .git at the root, children are checkouts) resolve to
// projectPath/<repo> when that directory is a git root, or to a child whose
// origin remote parses as owner/repo.
//
// owner/repo are used for remote matching; the named-child path uses repo only
// (common layout: …/deploys-app/api for deploys-app/api).
func ResolveLocalRepo(ctx context.Context, projectPath, owner, repo string) (string, error) {
	projectPath = strings.TrimSpace(projectPath)
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if projectPath == "" {
		return "", fmt.Errorf("empty project path")
	}
	if IsRepo(projectPath) {
		return projectPath, nil
	}
	if repo != "" {
		cand := filepath.Join(projectPath, repo)
		if IsRepo(cand) {
			return cand, nil
		}
	}
	// Scan immediate children for a matching origin remote (layout where the
	// folder name differs from the GitHub repo name).
	if owner != "" && repo != "" {
		if found, err := findChildByRemote(ctx, projectPath, owner, repo); err == nil && found != "" {
			return found, nil
		}
	}
	if repo != "" {
		return "", fmt.Errorf("no local checkout for %s/%s under %s (expected subdirectory %q)", owner, repo, projectPath, repo)
	}
	return "", fmt.Errorf("project path %s is not a git repository and no repo name was provided", projectPath)
}

func findChildByRemote(ctx context.Context, projectPath, owner, repo string) (string, error) {
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(owner + "/" + repo)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		cand := filepath.Join(projectPath, e.Name())
		if !IsRepo(cand) {
			continue
		}
		raw, err := gitOutput(ctx, cand, "remote", "get-url", "origin")
		if err != nil {
			continue
		}
		if r, ok := parseGitHubRemote(raw); ok {
			if strings.EqualFold(r, want) {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("not found")
}

// parseGitHubRemote extracts "owner/repo" from common GitHub remote URL forms.
// Kept local so this package does not depend on config.
func parseGitHubRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimRight(remote, "/")
	low := strings.ToLower(remote)
	var rest string
	switch {
	case strings.HasPrefix(low, "git@github.com:"):
		rest = remote[len("git@github.com:"):]
	case strings.HasPrefix(low, "ssh://git@github.com/"):
		rest = remote[len("ssh://git@github.com/"):]
	case strings.HasPrefix(low, "https://github.com/"):
		rest = remote[len("https://github.com/"):]
	case strings.HasPrefix(low, "http://github.com/"):
		rest = remote[len("http://github.com/"):]
	case strings.HasPrefix(low, "https://www.github.com/"):
		rest = remote[len("https://www.github.com/"):]
	case strings.HasPrefix(low, "http://www.github.com/"):
		rest = remote[len("http://www.github.com/"):]
	default:
		return "", false
	}
	rest = strings.TrimSuffix(rest, ".git")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}
