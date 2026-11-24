package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/codetree"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

const (
	codeListCap  = 2000
	codeMaxLines = 10000
	codeFindCap  = 200
)

// commitHex is a Git object name, short or full, SHA-1 or SHA-256.
// Anything else, including a leading dash, never reaches git.
var commitHex = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// codeView is the source browser: one directory or one file.
type codeView struct {
	Ref           string
	Branches      []string
	Tags          []codeTag
	Commits       []codeCommit
	TipOID        string
	TipShort      string
	TreeOID       string
	TreeShort     string
	Pinned        bool
	Path          string
	Crumbs        []codeCrumb
	Entries       []codeEntry
	EntryTotal    int
	Shown         int // child rows rendered; the synthetic .. row is not included
	Clipped       bool
	Listed        bool
	ParentHref    string
	File          bool
	Empty         bool
	Binary        bool
	Symlink       bool
	LinkText      string
	Submodule     bool
	LFS           bool
	TooBig        bool
	TooMany       bool
	Missing       bool
	MissingObject bool
	// Note is a page error that is not one of the flags above (bad commit,
	// bad path, revision that did not resolve).
	Note         string
	Size         int64
	SizeText     string
	Mode         string
	BlobOID      string
	BlobShort    string
	Lines        []codeLine
	RawURL       string
	CanFetch     bool
	Query        string
	QueryNote    string
	QueryTotal   int
	QueryShown   int
	QueryClipped bool
	Matches      []codeEntry
}

type codeTag struct {
	Ref   string
	Label string
}

type codeCommit struct {
	SHA      string
	Short    string
	Subject  string
	Selected bool
}

type codeCrumb struct {
	Label string
	Href  string
	Last  bool
}

type codeEntry struct {
	Name string
	Href string
	Meta string
}

type codeLine struct {
	N      int
	Tokens []codeToken
}

type codeRev struct {
	// Ref is the dropdown value (origin/main, refs/tags/v1, or HEAD).
	Ref string
	// Qualified is what rev-parse receives. It is built from a listed ref,
	// never from the query string.
	Qualified string
}

func (s *Server) codeRun() codetree.Runner {
	if s != nil && s.codeRunner != nil {
		return s.codeRunner
	}
	return nil
}

func (s *Server) codePage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	d := s.basePage(ctx)
	d.Title = project + " · Code"
	d.IsCode = true
	d.Project = project
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	root, err := s.projectPath(project)
	if err != nil {
		return ctx.Status(http.StatusNotFound).Error(err.Error())
	}
	catalog, catErr := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	if catErr != nil {
		return ctx.Status(http.StatusBadRequest).Error(catErr.Error())
	}
	d.RepoCatalog = catalog
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	owner := strings.TrimSpace(ctx.FormValue("owner"))
	repo := strings.TrimSpace(ctx.FormValue("repo"))
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pickErr != nil {
		if d.Error == "" {
			d.Error = pickErr.Error()
		}
		return s.viewPage(ctx, "code", d)
	}
	d.ActiveOwner = active.Owner
	d.ActiveRepo = active.Repo
	repoPath, pathErr := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if pathErr != nil {
		if d.Error == "" {
			d.Error = pathErr.Error()
		}
		return s.viewPage(ctx, "code", d)
	}
	d.Code = s.loadCode(ctx, project, active, repoPath)
	if msg := codePageError(d.Code); msg != "" && d.Error == "" {
		d.Error = msg
	}
	return s.viewPage(ctx, "code", d)
}

func codePageError(v codeView) string {
	if v.Note != "" {
		return v.Note
	}
	switch {
	case v.Missing:
		if v.TreeShort == "" {
			return "This path is not in this revision."
		}
		return "This path is not in " + v.TreeShort + "."
	case v.MissingObject:
		return "Object is not in this clone."
	default:
		return ""
	}
}

func (s *Server) loadCode(ctx *hime.Context, project string, active config.GitHubRepoRef, repoPath string) codeView {
	v := codeView{CanFetch: true}
	branches, _ := gitworktree.OriginTrackingRefs(ctx.Context(), repoPath)
	tags, _ := codetree.TagRefs(ctx.Context(), s.codeRun(), repoPath)
	fallback := gitworktree.ResolveCommitsRef(ctx.Context(), repoPath, "")
	rev := pickCodeRef(branches, tags, strings.TrimSpace(ctx.FormValue("ref")), fallback)
	v.Ref = rev.Ref
	v.Branches = branches
	if rev.Ref == "HEAD" && !slices.Contains(branches, "HEAD") {
		v.Branches = append(v.Branches, "HEAD")
	}
	for _, tag := range tags {
		v.Tags = append(v.Tags, codeTag{Ref: tag, Label: strings.TrimPrefix(tag, "refs/tags/")})
	}
	requested, cErr := normalizeCommit(ctx.FormValue("commit"))
	if cErr != nil {
		v.Note = cErr.Error()
		// Keep the selectors. Do not open a tree for a commit git was not allowed to see.
		s.fillCodeCommits(ctx, repoPath, rev, "", &v)
		return v
	}
	tipOID, tipErr := codetree.ResolveCommit(ctx.Context(), s.codeRun(), repoPath, rev.Qualified)
	if tipErr != nil {
		v.Note = tipErr.Error()
		return v
	}
	v.TipOID = tipOID
	v.TipShort = shortOID(tipOID)
	treeOID := tipOID
	if requested != "" {
		oid, err := codetree.ResolveCommit(ctx.Context(), s.codeRun(), repoPath, requested)
		if err != nil {
			v.Note = err.Error()
			v.TreeShort = v.TipShort
			s.fillCodeCommits(ctx, repoPath, rev, "", &v)
			return v
		}
		treeOID = oid
		v.Pinned = true
	}
	v.TreeOID = treeOID
	v.TreeShort = shortOID(treeOID)
	s.fillCodeCommits(ctx, repoPath, rev, treeOID, &v)
	v.Query = strings.TrimSpace(ctx.FormValue("q"))

	clean, pathErr := codetree.CleanPath(ctx.FormValue("path"))
	if pathErr != nil {
		v.Note = "invalid path"
		return v
	}
	v.Path = clean
	v.Crumbs = codeCrumbs(project, active.Owner, active.Repo, rev.Ref, pinnedCommit(v), clean)
	if v.Query != "" {
		s.fillCodeMatches(ctx, &v, project, active, repoPath)
	}
	if clean != "" {
		v.ParentHref = codeURL(project, false, active.Owner, active.Repo, rev.Ref, pinnedCommit(v), parentPath(clean))
	}
	node, err := codetree.Read(ctx.Context(), s.codeRun(), repoPath, treeOID, clean)
	if err != nil {
		if errors.Is(err, codetree.ErrNotFound) {
			v.Missing = true
			return v
		}
		if errors.Is(err, codetree.ErrOutputCapped) {
			v.Clipped = true
			v.EntryTotal = -1
			return v
		}
		v.Note = err.Error()
		return v
	}
	if node.Dir {
		s.fillCodeDir(&v, project, active, rev.Ref, pinnedCommit(v), clean, node.Children)
		return v
	}
	s.fillCodeFile(ctx, &v, project, active, rev.Ref, repoPath, node)
	return v
}

func (s *Server) fillCodeCommits(ctx *hime.Context, repoPath string, rev codeRev, treeOID string, v *codeView) {
	list, err := ghpr.ListCommitsWith(ctx.Context(), s.ghRun(), repoPath, ghpr.CommitListOpts{
		Ref:   rev.Qualified,
		Limit: ghpr.DefaultCommitListLimit,
	})
	if err != nil {
		list = nil
	}
	if v.Pinned && treeOID != "" && !slices.ContainsFunc(list, func(c ghpr.CommitSummary) bool { return c.SHA == treeOID }) {
		one, oErr := ghpr.ListCommitsWith(ctx.Context(), s.ghRun(), repoPath, ghpr.CommitListOpts{Ref: treeOID, Limit: 1})
		extra := ghpr.CommitSummary{SHA: treeOID}
		if oErr == nil && len(one) > 0 {
			extra = one[0]
		}
		list = append([]ghpr.CommitSummary{extra}, list...)
	}
	for _, c := range list {
		v.Commits = append(v.Commits, codeCommit{
			SHA:      c.SHA,
			Short:    shortOID(c.SHA),
			Subject:  clipRunes(c.Subject, 72),
			Selected: v.Pinned && c.SHA == treeOID,
		})
	}
}

func (s *Server) fillCodeDir(v *codeView, project string, active config.GitHubRepoRef, ref, commit, dir string, kids []codetree.Entry) {
	v.Listed = true
	v.EntryTotal = len(kids)
	v.Shown = len(kids)
	if len(kids) > codeListCap {
		v.Clipped = true
		kids = kids[:codeListCap]
		v.Shown = len(kids)
	}
	pin := commit
	if dir != "" {
		v.Entries = append(v.Entries, codeEntry{
			Name: "..",
			Href: v.ParentHref,
		})
	}
	for _, e := range kids {
		rel := e.Name
		if dir != "" {
			rel = dir + "/" + e.Name
		}
		row := codeEntry{Name: e.Name}
		switch e.Kind() {
		case codetree.KindDir:
			row.Href = codeURL(project, false, active.Owner, active.Repo, ref, pin, rel)
		case codetree.KindSubmodule:
			row.Meta = "submodule " + shortOID(e.OID)
		case codetree.KindSymlink:
			row.Href = codeURL(project, false, active.Owner, active.Repo, ref, pin, rel)
			row.Meta = "symlink"
		default:
			row.Href = codeURL(project, false, active.Owner, active.Repo, ref, pin, rel)
			row.Meta = formatCodeSize(e.Size)
		}
		v.Entries = append(v.Entries, row)
	}
}

func (s *Server) fillCodeFile(ctx *hime.Context, v *codeView, project string, active config.GitHubRepoRef, ref, repoPath string, node codetree.Node) {
	e := node.Entry
	v.File = true
	v.Mode = e.Mode
	v.BlobOID = e.OID
	v.BlobShort = shortOID(e.OID)
	v.Size = e.Size
	v.SizeText = formatCodeSize(e.Size)
	v.Symlink = node.Symlink
	v.Submodule = node.Submodule
	if node.Submodule {
		return
	}
	if e.Size > codetree.MaxBlobBytes {
		v.TooBig = true
		return
	}
	body, err := codetree.ReadBlob(ctx.Context(), s.codeRun(), repoPath, e.OID, e.Size, codetree.MaxBlobBytes)
	if errors.Is(err, codetree.ErrTooBig) {
		v.TooBig = true
		return
	}
	if err != nil {
		v.MissingObject = true
		return
	}
	v.RawURL = codeURL(project, true, active.Owner, active.Repo, ref, pinnedCommit(*v), v.Path)
	if node.Symlink {
		if !codetree.Binary(body) {
			v.LinkText = string(body)
		}
		return
	}
	if codetree.Binary(body) {
		v.Binary = true
		return
	}
	if codetree.LFSPointer(body) {
		v.LFS = true
	}
	if len(body) == 0 {
		v.Empty = true
		return
	}
	parts, tooMany := splitCodeLines(string(body))
	if tooMany {
		v.TooMany = true
		return
	}
	if v.LFS {
		v.Lines = plainCodeLines(parts)
		return
	}
	v.Lines = highlightParts(v.Path, parts)
}

func (s *Server) fillCodeMatches(ctx *hime.Context, v *codeView, project string, active config.GitHubRepoRef, repoPath string) {
	paths, err := codetree.ListPaths(ctx.Context(), s.codeRun(), repoPath, v.TreeOID)
	if err != nil {
		if errors.Is(err, codetree.ErrOutputCapped) {
			v.QueryNote = "This tree is too large to search."
			return
		}
		v.QueryNote = "Could not list this revision."
		return
	}
	shown, total := filterCodePaths(paths, v.Query)
	v.QueryTotal = total
	v.QueryShown = len(shown)
	v.QueryClipped = total > len(shown)
	pin := pinnedCommit(*v)
	for _, p := range shown {
		v.Matches = append(v.Matches, codeEntry{
			Name: p,
			Href: codeURL(project, false, active.Owner, active.Repo, v.Ref, pin, p),
		})
	}
}

func filterCodePaths(paths []string, q string) ([]string, int) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil, 0
	}
	var shown []string
	total := 0
	for _, p := range paths {
		if !strings.Contains(strings.ToLower(p), q) {
			continue
		}
		total++
		if len(shown) < codeFindCap {
			shown = append(shown, p)
		}
	}
	return shown, total
}

func pinnedCommit(v codeView) string {
	if v.Pinned {
		return v.TreeOID
	}
	return ""
}

func (s *Server) codeRaw(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	root, err := s.projectPath(project)
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
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	repoPath, err := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	branches, _ := gitworktree.OriginTrackingRefs(ctx.Context(), repoPath)
	tags, _ := codetree.TagRefs(ctx.Context(), s.codeRun(), repoPath)
	rev := pickCodeRef(branches, tags, strings.TrimSpace(ctx.FormValue("ref")), gitworktree.ResolveCommitsRef(ctx.Context(), repoPath, ""))
	requested, err := normalizeCommit(ctx.FormValue("commit"))
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	treeRev := rev.Qualified
	if requested != "" {
		treeRev = requested
	}
	treeOID, err := codetree.ResolveCommit(ctx.Context(), s.codeRun(), repoPath, treeRev)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	clean, err := codetree.CleanPath(ctx.FormValue("path"))
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error("invalid path")
	}
	if clean == "" {
		return ctx.Status(http.StatusBadRequest).Error("path is required")
	}
	node, err := codetree.Read(ctx.Context(), s.codeRun(), repoPath, treeOID, clean)
	if errors.Is(err, codetree.ErrNotFound) {
		return ctx.Status(http.StatusNotFound).Error("path not found")
	}
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	if !node.File && !node.Symlink {
		return ctx.Status(http.StatusBadRequest).Error("not a file")
	}
	if node.Entry.Size > codetree.MaxBlobBytes {
		return ctx.Status(http.StatusRequestEntityTooLarge).Error("file is too large to download")
	}
	body, err := codetree.ReadBlob(ctx.Context(), s.codeRun(), repoPath, node.Entry.OID, node.Entry.Size, codetree.MaxBlobBytes)
	if errors.Is(err, codetree.ErrTooBig) {
		return ctx.Status(http.StatusRequestEntityTooLarge).Error("file is too large to download")
	}
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error("object is not in this clone")
	}
	w := ctx.ResponseWriter()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if codetree.Binary(body) {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment; filename=\""+codeAttachmentName(clean)+"\"")
	} else {
		// Never text/html. A file in the repo must not run as a page on this origin.
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

func (s *Server) postCodeFetch(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	ref := strings.TrimSpace(ctx.PostFormValue("ref"))
	commit := strings.TrimSpace(ctx.PostFormValue("commit"))
	p := strings.TrimSpace(ctx.PostFormValue("path"))
	root, err := s.projectPath(project)
	if err != nil {
		return s.codeRedirect(ctx, project, owner, repo, ref, commit, p, "", err)
	}
	catalog, _ := s.cfg.ProjectRepoCatalogWith(ctx.Context(), project, nil)
	active, pickErr := config.ResolveRepoPicker(catalog, owner, repo)
	if pickErr != nil {
		return s.codeRedirect(ctx, project, owner, repo, ref, commit, p, "", pickErr)
	}
	repoPath, err := gitworktree.ResolveLocalRepo(ctx.Context(), root, active.Owner, active.Repo)
	if err != nil {
		return s.codeRedirect(ctx, project, active.Owner, active.Repo, ref, commit, p, "", err)
	}
	err = ghpr.FetchWith(ctx.Context(), s.ghRun(), repoPath)
	s.auditAction(ctx, audit.ActionGitFetch, err, map[string]any{
		"project": project, "owner": active.Owner, "repo": active.Repo,
	})
	if err != nil {
		return s.codeRedirect(ctx, project, active.Owner, active.Repo, ref, commit, p, "", err)
	}
	gitworktree.NoteFetched(repoPath)
	return s.codeRedirect(ctx, project, active.Owner, active.Repo, ref, commit, p, "Fetched remotes (full history)", nil)
}

func (s *Server) codeRedirect(ctx *hime.Context, project, owner, repo, ref, commit, path, okMsg string, err error) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if ref != "" {
		q.Set("ref", ref)
	}
	if commit != "" {
		q.Set("commit", commit)
	}
	if path != "" {
		q.Set("path", path)
	}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}
	u := "/projects/" + url.PathEscape(project) + "/code"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return ctx.Redirect(u)
}

func pickCodeRef(branches, tags []string, requested, fallback string) codeRev {
	if rev, ok := matchBranch(branches, requested); ok {
		return rev
	}
	if rev, ok := matchTag(tags, requested); ok {
		return rev
	}
	if rev, ok := matchBranch(branches, fallback); ok {
		return rev
	}
	return codeRev{Ref: "HEAD", Qualified: "HEAD"}
}

func matchBranch(branches []string, want string) (codeRev, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return codeRev{}, false
	}
	for _, b := range branches {
		if b != want {
			continue
		}
		rest, ok := strings.CutPrefix(b, "origin/")
		if !ok || rest == "" || strings.Contains(rest, "..") || strings.ContainsAny(rest, " \t\n\r^:~\\?*[") {
			return codeRev{}, false
		}
		return codeRev{Ref: b, Qualified: "refs/remotes/" + b}, true
	}
	return codeRev{}, false
}

func matchTag(tags []string, want string) (codeRev, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return codeRev{}, false
	}
	for _, tag := range tags {
		if tag != want {
			continue
		}
		rest, ok := strings.CutPrefix(tag, "refs/tags/")
		if !ok || rest == "" || strings.Contains(rest, "..") || strings.ContainsAny(rest, " \t\n\r^:~\\?*[") {
			continue
		}
		return codeRev{Ref: tag, Qualified: tag}, true
	}
	return codeRev{}, false
}

func normalizeCommit(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !commitHex.MatchString(s) {
		return "", errors.New("invalid commit")
	}
	return s, nil
}

func codeURL(project string, raw bool, owner, repo, ref, commit, filePath string) string {
	u := "/projects/" + url.PathEscape(project) + "/code"
	if raw {
		u += "/raw"
	}
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if ref != "" {
		q.Set("ref", ref)
	}
	if commit != "" {
		q.Set("commit", commit)
	}
	if filePath != "" {
		q.Set("path", filePath)
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

func codeCrumbs(project, owner, repo, ref, commit, filePath string) []codeCrumb {
	root := codeCrumb{Label: "root", Href: codeURL(project, false, owner, repo, ref, commit, "")}
	if filePath == "" {
		root.Last = true
		root.Href = ""
		return []codeCrumb{root}
	}
	out := []codeCrumb{root}
	var b strings.Builder
	parts := strings.Split(filePath, "/")
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(p)
		last := i == len(parts)-1
		crumb := codeCrumb{Label: p, Last: last}
		if !last {
			crumb.Href = codeURL(project, false, owner, repo, ref, commit, b.String())
		}
		out = append(out, crumb)
	}
	return out
}

func parentPath(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

func shortOID(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j] + "…"
		}
		i++
	}
	return s
}

func formatCodeSize(n int64) string {
	if n < 0 {
		return ""
	}
	const kb = 1024
	switch {
	case n >= kb*kb:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(kb*kb))
	case n >= kb:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func splitCodeLines(body string) ([]string, bool) {
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		return nil, false
	}
	n := strings.Count(body, "\n") + 1
	if n > codeMaxLines {
		return nil, true
	}
	return strings.Split(body, "\n"), false
}

func codeAttachmentName(p string) string {
	name := path.Base(p)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}
