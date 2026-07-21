package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/ghpr"
)

// Diff review UI (commit / session / PR diffs): the page renders a file index
// only; hunks stream per file via fragment endpoints, so huge changesets never
// build a huge DOM and caps apply per file instead of per changeset.

// bigFileLines gates auto-loading: above this many changed lines a file loads
// only on explicit click.
const bigFileLines = 500

type diffFileView struct {
	ghpr.FileStat
	Idx       int
	FragURL   string
	DirName   string // directory part, no trailing slash ("" for root)
	Base      string // file name
	Big       bool
	Generated bool
	AutoLoad  bool // lazy-load when scrolled into view
	Collapsed bool // initially folded (deleted / generated)
}

// LoadNote is the placeholder text for click-to-load bodies.
func (v diffFileView) LoadNote() string {
	n := v.Adds + v.Dels
	switch {
	case v.Status == "D":
		return fmt.Sprintf("File deleted — %d lines.", n)
	case v.Generated:
		return fmt.Sprintf("Generated file — %d changed lines, skipped by default.", n)
	default:
		return fmt.Sprintf("Large diff — %d changed lines.", n)
	}
}

type diffFileGroup struct {
	Dir   string
	Files []diffFileView
}

type diffReviewData struct {
	Files     []diffFileView
	Groups    []diffFileGroup
	TotalAdds int
	TotalDels int
	Truncated bool
	ReviewKey string // localStorage key for viewed-tracking
}

// fileFragData feeds the per-file hunks fragment.
type fileFragData struct {
	Path      string
	Hunks     []ghpr.RenderedHunk
	Truncated bool
	Err       string
}

var generatedBasenames = map[string]bool{
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
	"go.sum":            true,
	"Cargo.lock":        true,
	"Gemfile.lock":      true,
	"composer.lock":     true,
	"poetry.lock":       true,
	"uv.lock":           true,
}

func isGeneratedPath(p string) bool {
	base := path.Base(p)
	if generatedBasenames[base] {
		return true
	}
	for _, suf := range []string{".pb.go", "_generated.go", ".gen.go", ".min.js", ".min.css"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	for _, dir := range []string{"vendor/", "node_modules/", "dist/"} {
		if strings.HasPrefix(p, dir) || strings.Contains(p, "/"+dir) {
			return true
		}
	}
	return false
}

// buildDiffReview turns a DiffIndex into view data. fragURL builds the
// per-file fragment URL for this surface.
func buildDiffReview(idx ghpr.DiffIndex, reviewKey string, fragURL func(f ghpr.FileStat) string) *diffReviewData {
	r := &diffReviewData{
		TotalAdds: idx.TotalAdds,
		TotalDels: idx.TotalDels,
		Truncated: idx.Truncated,
		ReviewKey: reviewKey,
	}
	r.Files = make([]diffFileView, 0, len(idx.Files))
	for i, f := range idx.Files {
		v := diffFileView{
			FileStat:  f,
			Idx:       i,
			FragURL:   fragURL(f),
			Base:      path.Base(f.Path),
			Big:       f.Adds+f.Dels > bigFileLines,
			Generated: isGeneratedPath(f.Path),
		}
		if d := path.Dir(f.Path); d != "." {
			v.DirName = d
		}
		v.AutoLoad = !v.Big && !v.Generated && !f.Binary && f.Status != "D"
		v.Collapsed = v.Generated || f.Status == "D"
		r.Files = append(r.Files, v)
	}
	// Rail groups in first-seen directory order. Not adjacency-based: git
	// sorts subdirectories between a parent's own files (internal/web/live.go
	// < internal/web/templates/x < internal/web/web.go), which would split
	// the parent into two groups.
	groupIdx := map[string]int{}
	for _, v := range r.Files {
		i, ok := groupIdx[v.DirName]
		if !ok {
			i = len(r.Groups)
			groupIdx[v.DirName] = i
			r.Groups = append(r.Groups, diffFileGroup{Dir: v.DirName})
		}
		r.Groups[i].Files = append(r.Groups[i].Files, v)
	}
	return r
}

// fragQuery encodes the shared per-file query params.
func fragQuery(f ghpr.FileStat, extra url.Values) string {
	q := url.Values{}
	for k, vs := range extra {
		q[k] = vs
	}
	q.Set("path", f.Path)
	if f.OldPath != "" && f.OldPath != f.Path {
		q.Set("old", f.OldPath)
	}
	return q.Encode()
}

// cleanDiffPath validates a repo-relative file path from the query string.
// The path only ever follows a `--` pathspec separator, but reject traversal
// and pathspec magic anyway.
func cleanDiffPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > 1024 {
		return "", false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, ":") {
		return "", false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == ".." {
			return "", false
		}
	}
	return p, true
}

// fileFragFromDiff picks the requested file out of a parsed per-file diff.
func fileFragFromDiff(diff ghpr.Diff, reqPath string) *fileFragData {
	frag := &fileFragData{Path: reqPath, Truncated: diff.Truncated}
	for _, f := range diff.Files {
		if f.PathNew == reqPath || (f.PathNew == "" && f.PathOld == reqPath) {
			frag.Hunks = ghpr.RenderHunks(f)
			return frag
		}
	}
	// Rename fragments arrive under the new path; fall back to the only file.
	if len(diff.Files) == 1 {
		frag.Hunks = ghpr.RenderHunks(diff.Files[0])
	}
	return frag
}

// viewFileFrag renders the hunks fragment; git errors render inline so the
// card shows the failure instead of a broken swap.
func (s *Server) viewFileFrag(ctx *hime.Context, page string, diff ghpr.Diff, reqPath string, err error) error {
	frag := fileFragFromDiff(diff, reqPath)
	if err != nil {
		frag.Err = err.Error()
	}
	d := pageData{FileFrag: frag}
	return s.viewFragment(ctx, page, "diff_file_frag", d)
}

// commitDiffFile serves one file's hunks for the commit detail page.
func (s *Server) commitDiffFile(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	sha := strings.TrimSpace(ctx.PathValue("sha"))
	if sha == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing commit sha")
	}
	repoPath, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	reqPath, ok := cleanDiffPath(ctx.FormValue("path"))
	if !ok {
		return ctx.Status(http.StatusBadRequest).Error("invalid path")
	}
	oldPath, _ := cleanDiffPath(ctx.FormValue("old"))
	diff, diffErr := ghpr.ShowCommitFileWith(ctx.Context(), s.ghRun(), repoPath, sha, reqPath, oldPath, ghpr.FileCaps())
	return s.viewFileFrag(ctx, "commit_detail", diff, reqPath, diffErr)
}

// sessionDiffFile serves one file's hunks for the session worktree diff page.
func (s *Server) sessionDiffFile(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	ent, ok := s.sessions.Get(threadID)
	if !ok {
		return ctx.Status(http.StatusNotFound).Error("unknown session/thread")
	}
	cwd, project := s.resolveSessionDiffCwd(ent, threadID)
	if cwd == "" {
		return ctx.Status(http.StatusNotFound).Error(fmt.Sprintf("no git worktree found for this session (project=%q)", project))
	}
	base := s.sessionDiffBase(ctx.Context(), ent, cwd, ctx.FormValue("base"))
	reqPath, okP := cleanDiffPath(ctx.FormValue("path"))
	if !okP {
		return ctx.Status(http.StatusBadRequest).Error("invalid path")
	}
	oldPath, _ := cleanDiffPath(ctx.FormValue("old"))
	diff, diffErr := ghpr.WorktreeDiffFileWith(ctx.Context(), s.ghRun(), cwd, base, reqPath, oldPath, ghpr.FileCaps())
	return s.viewFileFrag(ctx, "diff", diff, reqPath, diffErr)
}

// prPatch fetches a PR's raw patch with a short-TTL cache: the page and its
// per-file fragments (dozens per scroll) would otherwise each re-download the
// entire patch from GitHub. Commit/session surfaces are local git and skip it.
const (
	prPatchTTL        = 60 * time.Second
	prPatchMaxEntries = 8
)

type prPatchEntry struct {
	raw []byte
	at  time.Time
}

func (s *Server) prPatch(ctx context.Context, cwd, selector string) ([]byte, error) {
	now := time.Now()
	s.prPatchMu.Lock()
	if e, ok := s.prPatches[selector]; ok && now.Sub(e.at) < prPatchTTL {
		raw := e.raw
		s.prPatchMu.Unlock()
		return raw, nil
	}
	s.prPatchMu.Unlock()

	raw, err := ghpr.PRPatchWith(ctx, s.ghRun(), cwd, selector)
	if err != nil {
		return nil, err
	}
	s.prPatchMu.Lock()
	if s.prPatches == nil {
		s.prPatches = map[string]prPatchEntry{}
	}
	for k, e := range s.prPatches {
		if now.Sub(e.at) >= prPatchTTL {
			delete(s.prPatches, k)
		}
	}
	if len(s.prPatches) >= prPatchMaxEntries {
		oldest, oldestAt := "", now
		for k, e := range s.prPatches {
			if e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.prPatches, oldest)
	}
	s.prPatches[selector] = prPatchEntry{raw: raw, at: now}
	s.prPatchMu.Unlock()
	return raw, nil
}

// prDiffFile serves one file's hunks for the PR diff page.
func (s *Server) prDiffFile(ctx *hime.Context) error {
	owner := strings.TrimSpace(ctx.PathValue("owner"))
	repo := strings.TrimSpace(ctx.PathValue("repo"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid PR number")
	}
	project := strings.TrimSpace(ctx.FormValue("project"))
	_, ref, cwd, err := s.resolveCatalogRepo(ctx.Context(), project, owner, repo)
	if err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	reqPath, ok := cleanDiffPath(ctx.FormValue("path"))
	if !ok {
		return ctx.Status(http.StatusBadRequest).Error("invalid path")
	}
	selector := fmt.Sprintf("https://github.com/%s/%s/pull/%d", ref.Owner, ref.Repo, n)
	var diff ghpr.Diff
	raw, diffErr := s.prPatch(ctx.Context(), cwd, selector)
	if diffErr == nil {
		diff = ghpr.ParseUnifiedDiff(ghpr.ExtractFilePatch(raw, reqPath), ghpr.FileCaps())
	}
	return s.viewFileFrag(ctx, "diff", diff, reqPath, diffErr)
}
