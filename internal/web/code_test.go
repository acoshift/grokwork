package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/codetree"
	"github.com/acoshift/grokwork/internal/config"
)

type codeFixture struct {
	srv   *Server
	cfg   *config.Config
	app   string
	first string
	head  string
}

func newCodeFixture(t *testing.T) codeFixture {
	t.Helper()
	srv, cfg, _ := testServer(t)
	root, ok := cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	app := filepath.Join(root, "app")
	api := filepath.Join(root, "api")
	if err := execGitInit(t, app); err != nil {
		t.Fatal(err)
	}
	if err := execGitInit(t, api); err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSpace(gitOut(t, app, "rev-parse", "HEAD"))

	write := func(name string, body []byte) {
		t.Helper()
		path := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README", []byte("two\n"))
	write("src/main.go", []byte("package main\n"))
	write("src/view.go", []byte(strings.Join([]string{
		"package view",
		"",
		`import "fmt"`,
		"",
		"// one",
		"// two",
		"// three",
		"// four",
		"func show() string {",
		`	return fmt.Sprintf("%s", "<script>alert(1)</script>")`,
		"}",
		"// end",
	}, "\n")+"\n"))
	write("page.html", []byte("<html><script>alert(1)</script></html>\n"))
	write(".hidden", []byte("dot\n"))
	write("*", []byte("star\n"))
	write("bin.dat", []byte{'a', 0, 'b'})
	write("big.bin", bytes.Repeat([]byte{'a'}, codetree.MaxBlobBytes+1))
	write("long.txt", []byte(strings.Repeat("x\n", codeMaxLines+1)))
	write("pointer", []byte("version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"))
	if err := os.Symlink("../outside", filepath.Join(app, "link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, app, "--literal-pathspecs", "add", "--", ".")
	gitIn(t, app, "update-index", "--add", "--cacheinfo", "160000,"+first+",mods/lib")
	gitIn(t, app, "commit", "-m", "files")
	head := strings.TrimSpace(gitOut(t, app, "rev-parse", "HEAD"))
	gitIn(t, app, "tag", "v1")
	gitIn(t, app, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, app, "update-ref", "refs/remotes/origin/feature", first)
	gitIn(t, app, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	if err := os.WriteFile(filepath.Join(api, "from-api.txt"), []byte("from api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, api, "add", "from-api.txt")
	gitIn(t, api, "commit", "-m", "api file")
	gitIn(t, api, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, api, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{
		{Owner: "acme", Repo: "app"},
		{Owner: "acme", Repo: "api"},
	}); err != nil {
		t.Fatal(err)
	}
	return codeFixture{srv: srv, cfg: cfg, app: app, first: first, head: head}
}

func TestCodeBrowsesRepoBranchAndCommit(t *testing.T) {
	fx := newCodeFixture(t)
	var args [][]string
	fx.srv.codeRunner = recordCodeRun(t, &args)

	root := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app")
	if !strings.Contains(root, `id="page-code"`) {
		t.Fatal("missing page marker")
	}
	assertNavActive(t, root, "Code")
	if !strings.Contains(root, `id="code-tree"`) {
		t.Fatal("root is not a directory listing")
	}
	assertCodePickersClear(t, root)
	if !strings.Contains(root, `id="code-fetch"`) || !strings.Contains(root, `hx-boost="false"`) {
		t.Fatal("fetch form missing")
	}
	mods := strings.Index(root, `path=mods&`)
	src := strings.Index(root, `path=src&`)
	hidden := strings.Index(root, ">.hidden<")
	if mods < 0 || src < mods || hidden < src {
		t.Fatalf("tree order want dirs then dotfile, mods=%d src=%d hidden=%d", mods, src, hidden)
	}
	if !strings.Contains(root, `value="origin/main" selected`) {
		t.Fatal("default ref is not origin/main")
	}
	if !strings.Contains(root, "Latest ("+fx.head[:7]+")") {
		t.Fatal("latest option missing tip")
	}

	dir := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=src")
	if !strings.Contains(dir, `>..</a>`) || !strings.Contains(dir, "main.go") {
		t.Fatalf("src listing missing parent or file: %s", dir)
	}
	var looked, listed bool
	for _, cmd := range args {
		if !hasArg(cmd, "ls-tree") {
			continue
		}
		if hasArg(cmd, "--") && hasArg(cmd, "src") && hasArg(cmd, "--literal-pathspecs") {
			looked = true
		}
		if !hasArg(cmd, "--") && hasArg(cmd, "--literal-pathspecs") {
			listed = true
		}
	}
	if !looked || !listed {
		t.Fatalf("directory open want lookup plus child list, args=%q", args)
	}

	file := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=src/main.go")
	view := codeViewText(t, file)
	if !strings.Contains(stripTags(view), "package main") {
		t.Fatalf("file view: %s", view)
	}
	if !strings.Contains(file, "/projects/proj/commits/"+fx.head+"?") {
		t.Fatal("file view missing commit link")
	}

	html := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=page.html")
	htmlView := codeViewText(t, html)
	if !strings.Contains(htmlView, "&lt;script&gt;alert(1)") {
		t.Fatalf("html was not escaped: %s", htmlView)
	}
	if strings.Contains(htmlView, "<script>alert(1)") {
		t.Fatal("html script ran in the source view")
	}

	star := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=%2A")
	if !strings.Contains(codeViewText(t, star), "star") {
		t.Fatal("literal path * was not opened")
	}

	link := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=link")
	if !strings.Contains(link, "Symlink to") || !strings.Contains(link, "../outside") {
		t.Fatalf("symlink: %s", link)
	}
	if strings.Contains(link, `id="code-tree"`) {
		t.Fatal("symlink was followed into a tree")
	}

	sub := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=mods")
	if !strings.Contains(sub, "submodule ") || !strings.Contains(sub, "<span>lib</span>") {
		t.Fatalf("submodule row: %s", sub)
	}
	if strings.Contains(sub, "path=mods/lib") || strings.Contains(sub, "path=mods%2Flib") {
		t.Fatal("submodule row is a link")
	}

	old := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&commit="+fx.first+"&path=README")
	if !strings.Contains(codeViewText(t, old), "hi") {
		t.Fatal("pinned commit did not show the old README")
	}
	missing := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&commit="+fx.first+"&path=page.html")
	if !strings.Contains(missing, "This path is not in "+fx.first[:7]) {
		t.Fatalf("missing path: %s", missing)
	}
	if strings.Contains(missing, `id="code-tree"`) || strings.Contains(missing, `id="code-file"`) {
		t.Fatal("missing path still rendered a tree or file")
	}

	feature := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=origin/feature&path=README")
	if !strings.Contains(codeViewText(t, feature), "hi") {
		t.Fatal("feature ref did not resolve to its own tip")
	}
	featureRoot := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=origin/feature")
	if strings.Contains(featureRoot, "page.html") {
		t.Fatal("feature listing included a file from main")
	}

	tag := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=refs/tags/v1")
	if !strings.Contains(tag, "page.html") || !strings.Contains(tag, `value="refs/tags/v1" selected`) {
		t.Fatal("tag ref did not select v1")
	}

	api := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=api")
	if !strings.Contains(api, "from-api.txt") || strings.Contains(api, "page.html") {
		t.Fatal("repo switch did not change the tree")
	}

	bin := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=bin.dat")
	if !strings.Contains(bin, "Binary file") || bytes.Contains([]byte(bin), []byte{0}) {
		t.Fatal("binary file was inlined")
	}
	big := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=big.bin")
	if !strings.Contains(big, "over the 1 MiB limit") || strings.Contains(big, ">Raw<") {
		t.Fatal("over-cap blob was offered for download")
	}
	long := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=long.txt")
	if !strings.Contains(long, "too many lines") || !strings.Contains(long, ">Raw<") || strings.Contains(long, `id="code-view"`) {
		t.Fatal("line cap did not keep the raw link")
	}
	lfs := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=pointer")
	if !strings.Contains(lfs, "Git LFS pointer") || !strings.Contains(codeViewText(t, lfs), "version https://git-lfs.github.com/spec/v1") {
		t.Fatal("lfs pointer was not shown as text")
	}

	detail := getCode(t, fx.srv, "/projects/proj/commits/"+fx.head+"?owner=acme&repo=app")
	want := "/projects/proj/code?owner=acme&amp;repo=app&amp;commit=" + fx.head
	if !strings.Contains(detail, want) || !strings.Contains(detail, ">Browse code</a>") {
		t.Fatalf("commit detail missing browse link %s", want)
	}
}

func TestCodeRawStaysPlain(t *testing.T) {
	fx := newCodeFixture(t)
	h := fx.srv.Handler()
	raw := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/projects/proj/code/raw?owner=acme&repo=app&"+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	html := raw("path=page.html")
	if html.Code != http.StatusOK {
		t.Fatalf("html raw status=%d body=%s", html.Code, html.Body.String())
	}
	if got := html.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("html content-type %q", got)
	}
	if html.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("html raw missing nosniff")
	}
	if strings.Contains(html.Header().Get("Content-Type"), "text/html") {
		t.Fatal("raw html served as text/html")
	}
	if !strings.Contains(html.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("raw html body was rewritten")
	}

	bin := raw("path=bin.dat")
	if bin.Code != http.StatusOK {
		t.Fatalf("bin status=%d", bin.Code)
	}
	if bin.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("bin content-type %q", bin.Header().Get("Content-Type"))
	}
	if !strings.Contains(bin.Header().Get("Content-Disposition"), `attachment; filename="bin.dat"`) {
		t.Fatalf("bin disposition %q", bin.Header().Get("Content-Disposition"))
	}
	if !bytes.Contains(bin.Body.Bytes(), []byte{0}) {
		t.Fatal("bin body lost the NUL")
	}

	big := raw("path=big.bin")
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big status=%d body=%s", big.Code, big.Body.String())
	}
	dir := raw("path=src")
	if dir.Code != http.StatusBadRequest {
		t.Fatalf("dir raw status=%d", dir.Code)
	}
	missing := raw("path=no/such")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing raw status=%d", missing.Code)
	}
	empty := raw("")
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), "path is required") {
		t.Fatalf("empty raw status=%d body=%s", empty.Code, empty.Body.String())
	}
}

func TestCodeRejectsBadRevisionAndPath(t *testing.T) {
	fx := newCodeFixture(t)
	var args [][]string
	fx.srv.codeRunner = recordCodeRun(t, &args)

	badCommit := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&commit=-abcdef1")
	if !strings.Contains(badCommit, "invalid commit") || strings.Contains(badCommit, `id="code-tree"`) {
		t.Fatalf("bad commit: %s", badCommit)
	}
	assertArgsOmit(t, args, "-abcdef1")

	args = nil
	ignored := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=refs/heads/main")
	if !strings.Contains(ignored, `value="origin/main" selected`) {
		t.Fatal("refs/heads/main was honored")
	}
	assertArgsOmit(t, args, "refs/heads")
	if strings.Contains(ignored, `class="flash err"`) {
		t.Fatalf("unknown ref errored: %s", ignored)
	}

	args = nil
	head := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=HEAD")
	if !strings.Contains(head, `value="origin/main" selected`) {
		t.Fatal("ref=HEAD was honored while origin refs exist")
	}
	assertArgsOmit(t, args, "HEAD")

	args = nil
	dash := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=-upload-pack=evil")
	if !strings.Contains(dash, "page.html") {
		t.Fatal("ignored ref did not fall back to the checkout primary")
	}
	assertArgsOmit(t, args, "-upload-pack")

	args = nil
	badPath := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=../etc/passwd")
	if !strings.Contains(badPath, "invalid path") || strings.Contains(badPath, `id="code-tree"`) {
		t.Fatalf("bad path: %s", badPath)
	}
	assertArgsOmit(t, args, "..")

	args = nil
	req := httptest.NewRequest(http.MethodGet, "/projects/proj/code/raw?owner=acme&repo=app&path=../etc/passwd", nil)
	w := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid path") {
		t.Fatalf("raw bad path status=%d body=%s", w.Code, w.Body.String())
	}
	assertArgsOmit(t, args, "..")
	for _, cmd := range args {
		if hasArg(cmd, "ls-tree") || hasArg(cmd, "cat-file") {
			t.Fatalf("bad path reached git read: %q", cmd)
		}
	}
}

func TestCodeRepoQueryCannotEscape(t *testing.T) {
	srv, _, dir := testServer(t)
	leak := filepath.Join(dir, "secret-leak")
	if err := os.MkdirAll(leak, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leak, "SECRET_CODE_LEAK"), []byte("SECRET_CODE_LEAK"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	srv.codeRunner = func(context.Context, string, int64, ...string) ([]byte, error) {
		called = true
		t.Fatal("code git ran for a rejected repo")
		return nil, nil
	}
	for _, repo := range []string{"../secret-leak", "..", "foo/bar"} {
		body := getCode(t, srv, "/projects/proj/code?owner=acme&repo="+url.QueryEscape(repo)+"&q=SECRET_CODE_LEAK")
		if strings.Contains(body, "SECRET_CODE_LEAK") || strings.Contains(body, `id="code-tree"`) {
			t.Fatalf("repo %q escaped: %s", repo, body)
		}
		if !strings.Contains(body, "single path element") {
			t.Fatalf("repo %q did not hit ResolveLocalRepo: %s", repo, body)
		}
	}
	if called {
		t.Fatal("runner was called")
	}
	if _, err := os.Stat(filepath.Join(dir, "secret-leak")); err != nil {
		t.Fatal(err)
	}
}

func TestCodeFetchRedirectsAndAudits(t *testing.T) {
	fx := newCodeFixture(t)
	var fetched bool
	fx.srv.ghRunner = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "rev-parse" {
			return []byte("false\n"), nil
		}
		if name == "git" && len(args) > 0 && args[0] == "fetch" {
			fetched = true
			if strings.Join(args, " ") != "fetch --all --prune" {
				t.Fatalf("fetch args %v", args)
			}
			return nil, nil
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	}
	form := url.Values{
		"owner":  {"acme"},
		"repo":   {"app"},
		"ref":    {"origin/main"},
		"commit": {fx.head},
		"path":   {"src"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/code/fetch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !fetched {
		t.Fatal("expected git fetch")
	}
	loc := w.Header().Get("Location")
	for _, want := range []string{"/projects/proj/code?", "owner=acme", "repo=app", "path=src", "commit=" + fx.head, "ref=origin"} {
		if !strings.Contains(loc, want) {
			t.Fatalf("Location %q missing %q", loc, want)
		}
	}
	raw, err := os.ReadFile(auditFile(t, fx.cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"action":"git.fetch"`) || !strings.Contains(string(raw), `"repo":"app"`) {
		t.Fatalf("audit: %s", raw)
	}

	fx.srv.ghRunner = nil
	flash := getCode(t, fx.srv, loc)
	if !strings.Contains(flash, "Fetched remotes") {
		t.Fatalf("missing flash: %s", flash)
	}
}

func TestCodeRequiresProjectAccess(t *testing.T) {
	open, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/projects/no-such/code", nil)
	w := httptest.NewRecorder()
	open.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown project status=%d", w.Code)
	}

	srv := twoProjectAuthServer(t)
	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sid}
	get := func(path string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if got := get("/projects/public/code"); got != http.StatusOK {
		t.Fatalf("member public status=%d", got)
	}
	if got := get("/projects/secret/code"); got != http.StatusForbidden {
		t.Fatalf("member secret status=%d", got)
	}
	if got := get("/projects/secret/code/raw?path=README"); got != http.StatusForbidden {
		t.Fatalf("member secret raw status=%d", got)
	}
	if got := get("/projects/missing/code"); got != http.StatusForbidden {
		t.Fatalf("unknown status=%d", got)
	}
}

func TestCodeHighlightsFilePreview(t *testing.T) {
	fx := newCodeFixture(t)
	page := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=src/view.go")
	view := codeViewText(t, page)
	if !strings.Contains(view, `class="tok tok-kw">package</span>`) || !strings.Contains(view, `class="tok tok-str">`) {
		t.Fatalf("go file missing distinct tokens: %s", view)
	}
	if !strings.Contains(view, "&lt;script&gt;alert(1)") || strings.Contains(view, "<script>alert(1)") {
		t.Fatalf("script text was not escaped in the preview: %s", view)
	}
	if !strings.Contains(view, `class="code-ln"`) || !strings.Contains(view, `class="code-dc"`) {
		t.Fatal("preview is missing the gutter")
	}
	if !strings.Contains(view, `id="L12"`) || !strings.Contains(view, `href="#L12"`) {
		t.Fatal("line hash missing")
	}
	for _, id := range []string{"code-search", "code-search-prev", "code-search-next", "code-line-jump", "code-line-go"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Fatalf("preview missing %s", id)
		}
	}
	if !strings.Contains(page, ">Previous<") || !strings.Contains(page, ">Next<") || !strings.Contains(page, ">Go<") {
		t.Fatal("find controls missing")
	}
	file := between(t, page, `id="code-file"`, "\n<style>")
	for _, bad := range []string{">Edit<", ">Save<", ">Commit<"} {
		if strings.Contains(file, bad) {
			t.Fatalf("preview has %s", bad)
		}
	}

	html := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=page.html")
	htmlView := codeViewText(t, html)
	if !strings.Contains(htmlView, "&lt;script&gt;alert(1)") || strings.Contains(htmlView, "<script>alert(1)") {
		t.Fatalf("html fixture: %s", htmlView)
	}
	if strings.Contains(htmlView, `class="tok `) {
		t.Fatal("unknown extension was highlighted")
	}

	for _, path := range []string{"bin.dat", "big.bin", "long.txt", "link"} {
		body := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path="+path)
		if strings.Contains(body, `id="code-view"`) || strings.Contains(body, `class="tok `) {
			t.Fatalf("%s rendered a highlighted preview", path)
		}
	}
	lfs := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&path=pointer")
	lfsView := codeViewText(t, lfs)
	if strings.Contains(lfsView, `class="tok `) || !strings.Contains(lfsView, "version https://git-lfs.github.com/spec/v1") {
		t.Fatal("lfs pointer was highlighted or dropped")
	}
}

func TestCodePathQueryUsesSelectedTree(t *testing.T) {
	fx := newCodeFixture(t)
	gitIn(t, fx.app, "checkout", "-b", "side", fx.first)
	sideFile := filepath.Join(fx.app, "only-side.txt")
	if err := os.WriteFile(sideFile, []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, fx.app, "add", "only-side.txt")
	gitIn(t, fx.app, "commit", "-m", "side only")
	side := strings.TrimSpace(gitOut(t, fx.app, "rev-parse", "HEAD"))
	gitIn(t, fx.app, "update-ref", "refs/remotes/origin/side", side)
	gitIn(t, fx.app, "checkout", "--detach", fx.head)

	if err := os.MkdirAll(filepath.Join(fx.app, "bulk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 201 {
		name := fmt.Sprintf("bulk/f%03d.txt", i)
		if err := os.WriteFile(filepath.Join(fx.app, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, fx.app, "add", "bulk")
	gitIn(t, fx.app, "commit", "-m", "bulk")
	gitIn(t, fx.app, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(fx.app, "worktree-only.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var args [][]string
	fx.srv.codeRunner = recordCodeRun(t, &args)
	mainHead := strings.TrimSpace(gitOut(t, fx.app, "rev-parse", "HEAD"))

	found := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&q=SRC/MAIN.GO")
	sec := between(t, found, `id="code-matches"`, "\n<style>")
	if !strings.Contains(sec, ">src/main.go</a>") {
		t.Fatalf("selected commit missing src/main.go: %s", sec)
	}
	if strings.Contains(sec, "only-side.txt") || strings.Contains(sec, "worktree-only.txt") {
		t.Fatalf("query leaked another branch or the work tree: %s", sec)
	}
	var listed bool
	for _, cmd := range args {
		if !hasArg(cmd, "ls-tree") || !hasArg(cmd, "-r") || !hasArg(cmd, "--name-only") {
			continue
		}
		if hasArg(cmd, mainHead) {
			listed = true
		}
		for _, arg := range cmd {
			if strings.Contains(strings.ToLower(arg), "src/main") || strings.Contains(arg, "SRC") {
				t.Fatalf("query text reached git: %q", cmd)
			}
		}
	}
	if !listed {
		t.Fatalf("path query did not list the resolved commit: %q", args)
	}

	args = nil
	sidePage := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&ref=origin/side&q=only-side")
	sideSec := between(t, sidePage, `id="code-matches"`, "\n<style>")
	if !strings.Contains(sideSec, ">only-side.txt</a>") || strings.Contains(sideSec, "page.html") || strings.Contains(sideSec, "bulk/f") {
		t.Fatalf("side tree: %s", sideSec)
	}

	absent := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&q=only-side.txt")
	absentSec := between(t, absent, `id="code-matches"`, "\n<style>")
	if strings.Contains(absentSec, "only-side.txt") || !strings.Contains(absentSec, "No files match") {
		t.Fatalf("main tree included a side-only file: %s", absentSec)
	}
	dirty := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&q=worktree-only.txt")
	dirtySec := between(t, dirty, `id="code-matches"`, "\n<style>")
	if strings.Contains(dirtySec, "worktree-only.txt") || !strings.Contains(dirtySec, "No files match") {
		t.Fatalf("work tree file matched: %s", dirtySec)
	}

	clipped := getCode(t, fx.srv, "/projects/proj/code?owner=acme&repo=app&q=bulk/f")
	clipSec := between(t, clipped, `id="code-matches"`, "\n<style>")
	if !strings.Contains(clipSec, "Showing 200 of 201.") || strings.Count(clipSec, "bulk/f") != 200 {
		t.Fatalf("clip: count=%d body=%s", strings.Count(clipSec, "bulk/f"), clipSec)
	}
}

func getCode(t *testing.T, srv *Server, raw string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, raw, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", raw, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func between(t *testing.T, body, start, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("missing %s", start)
	}
	rest := body[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("missing %s after %s", end, start)
	}
	return rest[:j]
}

func stripTags(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return b.String()
		}
		s = s[i+j+1:]
	}
}

func codeViewText(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `id="code-view"`)
	if i < 0 {
		t.Fatalf("no code view: %s", body)
	}
	rest := body[i:]
	j := strings.Index(rest, "</pre>")
	if j < 0 {
		t.Fatal("code view not closed")
	}
	return rest[:j]
}

func assertCodePickersClear(t *testing.T, body string) {
	t.Helper()
	i := strings.Index(body, `sel.addEventListener('change'`)
	if i < 0 {
		t.Fatal("repo change listener missing")
	}
	rest := body[i:]
	j := strings.Index(rest, `refEl.addEventListener('change'`)
	if j < 0 {
		t.Fatal("ref change listener missing")
	}
	repoChange := rest[:j]
	for _, id := range []string{"code-ref", "code-commit", "code-path", "code-q"} {
		if !strings.Contains(repoChange, `getElementById('`+id+`')`) || !strings.Contains(repoChange, `disabled=true`) {
			t.Fatalf("repo change must disable %s before submit: %s", id, repoChange)
		}
	}
	refChange := rest[j:]
	if end := strings.Index(refChange, `form.addEventListener`); end >= 0 {
		refChange = refChange[:end]
	}
	if !strings.Contains(refChange, `getElementById('code-commit')`) || !strings.Contains(refChange, `disabled=true`) {
		t.Fatalf("ref change must disable commit: %s", refChange)
	}
	if strings.Contains(refChange, "code-path") || strings.Contains(refChange, "code-ref") || strings.Contains(refChange, "code-q") {
		t.Fatalf("ref change must keep path: %s", refChange)
	}
}

func recordCodeRun(t *testing.T, dst *[][]string) codetree.Runner {
	t.Helper()
	return func(ctx context.Context, dir string, maxOut int64, args ...string) ([]byte, error) {
		*dst = append(*dst, append([]string(nil), args...))
		return codetree.Run(ctx, dir, maxOut, args...)
	}
}

func assertArgsOmit(t *testing.T, args [][]string, bad string) {
	t.Helper()
	for _, cmd := range args {
		for _, arg := range cmd {
			if strings.Contains(arg, bad) {
				t.Fatalf("git arg %q contains %q in %q", arg, bad, cmd)
			}
		}
	}
}

func hasArg(cmd []string, want string) bool {
	for _, arg := range cmd {
		if arg == want {
			return true
		}
	}
	return false
}

func auditFile(t *testing.T, dataDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dataDir, "audit", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("audit files %v", matches)
	}
	return matches[0]
}
