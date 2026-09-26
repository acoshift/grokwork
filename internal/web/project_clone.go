package web

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

const cloneTimeout = 10 * time.Minute

func (s *Server) addProjectFromPath(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	path := ctx.PostFormValue("path")
	err := s.cfg.AddProject(name, path)
	s.auditAction(ctx, audit.ActionConfigAddProject, err, map[string]any{"name": name, "via": "path"})
	if err != nil {
		return s.projectNewRedirect(ctx, err)
	}
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added project %q", name), nil)
}

func (s *Server) addProjectFromClone(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	remote := strings.TrimSpace(ctx.PostFormValue("remote"))
	branch := strings.TrimSpace(ctx.PostFormValue("branch"))

	var parsed gitworktree.CloneRemote
	fail := func(err error) error {
		err = fmt.Errorf("%s", gitworktree.ScrubCloneText(err.Error()))
		detail := map[string]any{"name": name, "via": "clone"}
		if parsed.Spec != "" {
			detail["remote"] = parsed.AuditLabel()
		}
		s.auditAction(ctx, audit.ActionConfigAddProject, err, detail)
		return s.projectNewRedirect(ctx, err)
	}

	if err := validCloneProjectName(name); err != nil {
		return fail(err)
	}
	var err error
	parsed, err = gitworktree.ParseCloneRemote(remote)
	if err != nil {
		return fail(err)
	}

	s.cloneMu.Lock()
	defer s.cloneMu.Unlock()

	root, err := resolveReposRoot(s.cfg.ReposRoot())
	if err != nil {
		return fail(err)
	}
	dest, err := cloneDest(root, name)
	if err != nil {
		return fail(err)
	}
	if err := s.rejectCloneTarget(name, dest); err != nil {
		return fail(err)
	}

	incoming := filepath.Join(root, ".incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return fail(err)
	}
	temp, err := os.MkdirTemp(incoming, "c-")
	if err != nil {
		return fail(err)
	}
	clone := s.cloneRepo
	if clone == nil {
		clone = gitworktree.CloneAt
	}
	cctx, cancel := context.WithTimeout(ctx.Context(), cloneTimeout)
	defer cancel()
	if err := clone(cctx, parsed.Spec, temp, branch); err != nil {
		removeCloneDir(incoming, temp)
		return fail(err)
	}
	if !gitworktree.IsRepo(temp) {
		removeCloneDir(incoming, temp)
		return fail(fmt.Errorf("clone did not produce a git repository"))
	}
	if err := os.Rename(temp, dest); err != nil {
		removeCloneDir(incoming, temp)
		return fail(fmt.Errorf("checkout path already exists"))
	}
	if !gitworktree.IsRepo(dest) {
		removeCloneDir(root, dest)
		return fail(fmt.Errorf("clone did not produce a git repository"))
	}
	if err := s.cfg.AddProject(name, dest); err != nil {
		removeCloneDir(root, dest)
		return fail(err)
	}
	s.auditAction(ctx, audit.ActionConfigAddProject, nil, map[string]any{
		"name": name, "via": "clone", "remote": parsed.AuditLabel(),
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added project %q", name), nil)
}

func (s *Server) projectNewRedirect(ctx *hime.Context, err error) error {
	msg := "could not add project"
	if err != nil {
		msg = gitworktree.ScrubCloneText(err.Error())
		if msg == "" {
			msg = "could not add project"
		}
	}
	q := url.Values{}
	q.Set("err", msg)
	return ctx.Redirect(ctx.Route("config.projectNew") + "?" + q.Encode())
}

func validCloneProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("project name must be at most 64 characters")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") || filepath.Base(name) != name {
		return fmt.Errorf("project name must be a single directory name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) || strings.ContainsRune(`/\:?*[]^~`, r) {
			return fmt.Errorf("project name must be a single directory name")
		}
	}
	return nil
}

func resolveReposRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("data directory is not configured")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func cloneDest(root, name string) (string, error) {
	dest := filepath.Join(root, name)
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel != name {
		return "", fmt.Errorf("invalid clone destination")
	}
	return dest, nil
}

func (s *Server) rejectCloneTarget(name, dest string) error {
	if _, ok := s.cfg.ProjectPath(name); ok {
		return fmt.Errorf("project %q already exists", name)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("checkout path already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	inside, err := dirInside(s.cfg.WorktreesRoot(), dest)
	if err != nil {
		return err
	}
	if inside {
		return fmt.Errorf("clone directory collides with the worktree directory")
	}
	for _, p := range s.cfg.Snapshot().Projects {
		if pathsSame(p.Path, dest) {
			return fmt.Errorf("checkout path already exists")
		}
	}
	return nil
}

func dirInside(parent, child string) (bool, error) {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == "" || child == "" {
		return false, fmt.Errorf("empty path")
	}
	if p, err := filepath.EvalSymlinks(parent); err == nil {
		parent = p
	}
	if c, err := filepath.EvalSymlinks(child); err == nil {
		child = c
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func pathsSame(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	ar, aerr := filepath.EvalSymlinks(a)
	br, berr := filepath.EvalSymlinks(b)
	return aerr == nil && berr == nil && ar == br
}

// removeCloneDir deletes dir only when it is a real directory inside parent.
// A symlink is left alone so cleanup cannot follow it out of the clone root.
func removeCloneDir(parent, dir string) {
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return
	}
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return
	}
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(parentReal, dirReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	_ = os.RemoveAll(dir)
}
