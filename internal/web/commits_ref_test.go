package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
)

func TestCommitsRefDropdownFollowsRepoPrimary(t *testing.T) {
	srv, cfg, _ := testServer(t)
	root, ok := cfg.ProjectPath("proj")
	if !ok {
		t.Fatal("proj path")
	}
	if gitworktree.IsRepo(root) {
		t.Fatal("parent must not be a git root")
	}
	appDir := filepath.Join(root, "app")
	apiDir := filepath.Join(root, "api")
	bothDir := filepath.Join(root, "both")
	for _, dir := range []string{appDir, apiDir, bothDir} {
		if err := execGitInit(t, dir); err != nil {
			t.Fatal(err)
		}
	}
	// app primary is main. api primary is master and has no origin/main.
	// both has both names; origin/HEAD points at master.
	gitIn(t, appDir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, appDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitIn(t, apiDir, "update-ref", "refs/remotes/origin/master", "HEAD")
	gitIn(t, apiDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	gitIn(t, bothDir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, bothDir, "update-ref", "refs/remotes/origin/master", "HEAD")
	gitIn(t, bothDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{
		{Owner: "acme", Repo: "app"},
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "both"},
	}); err != nil {
		t.Fatal(err)
	}

	var logRef string
	oneCommit := []byte("abcdef0123456789abcdef0123456789abcdef01\x1fListed\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\n")
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "log" {
			logRef = args[len(args)-1]
			return oneCommit, nil
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	}
	h := srv.Handler()
	get := func(raw string) string {
		t.Helper()
		logRef = ""
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", raw, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, `class="flash err"`) || strings.Contains(body, "unknown revision") {
			t.Fatalf("GET %s showed a missing-ref error: %s", raw, body)
		}
		if !strings.Contains(body, `<select id="ref" name="ref"`) {
			t.Fatalf("GET %s ref control is not a dropdown: %s", raw, body)
		}
		if strings.Contains(body, `type="text" name="ref"`) {
			t.Fatalf("GET %s still has a ref text input", raw)
		}
		return body
	}

	const api = "/projects/proj/commits?owner=acme&repo=api"
	body := get(api + "&ref=origin/main")
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("carried origin/main logged %q", logRef)
	}
	if strings.Contains(body, `value="origin/main"`) {
		t.Fatalf("master checkout offered origin/main: %s", body)
	}
	if !strings.Contains(body, `type="hidden" name="ref" value="origin/master"`) {
		t.Fatalf("fetch form did not keep resolved ref: %s", body)
	}
	assertRepoPickOmitsRef(t, body)

	body = get(api)
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("omitted ref logged %q", logRef)
	}

	if err := cfg.SetProjectPrimaryBranch("proj", "main"); err != nil {
		t.Fatal(err)
	}
	body = get(api)
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("project primary main on master checkout logged %q", logRef)
	}
	if strings.Contains(body, `class="flash err"`) {
		t.Fatalf("project primary main errored: %s", body)
	}

	// primaryBranch stays main. both has origin/main and origin/master, and
	// origin/HEAD points at master. Empty ref and a ref that is not a commit
	// here both follow origin/HEAD, not the project primary.
	const both = "/projects/proj/commits?owner=acme&repo=both"
	body = get(both)
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("both with no ref, primaryBranch=main logged %q", logRef)
	}
	if strings.Contains(body, `<option value="origin/main" selected>`) {
		t.Fatal("project primary main selected on a master checkout that also has main")
	}
	body = get(both + "&ref=origin/does-not-exist")
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("both with missing ref, primaryBranch=main logged %q", logRef)
	}
	if strings.Contains(body, `class="flash err"`) || strings.Contains(body, "unknown revision") {
		t.Fatalf("missing ref errored while primaryBranch=main: %s", body)
	}

	body = get(api + "&ref=HEAD")
	assertSelectedRef(t, body, "HEAD")
	if logRef != "HEAD" {
		t.Fatalf("HEAD logged %q", logRef)
	}
	if !strings.Contains(body, `<option value="origin/master">origin/master</option>`) {
		t.Fatalf("HEAD page missing origin/master option: %s", body)
	}

	body = get(api + "&ref=origin/master")
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("explicit origin/master logged %q", logRef)
	}
	if strings.Contains(body, `value="HEAD"`) {
		t.Fatalf("existing branch ref should not add HEAD: %s", body)
	}

	// Previous ref name also exists here. With no ref (what a repo change
	// posts), the checkout primary is origin/master, not origin/main.
	body = get("/projects/proj/commits?owner=acme&repo=both")
	assertSelectedRef(t, body, "origin/master")
	if logRef != "origin/master" {
		t.Fatalf("both with no ref logged %q", logRef)
	}
	if !strings.Contains(body, `<option value="origin/main">origin/main</option>`) {
		t.Fatalf("both missing origin/main option: %s", body)
	}
	if strings.Contains(body, `<option value="origin/main" selected>`) {
		t.Fatal("both selected origin/main")
	}

	// A later explicit choice of an existing ref still lists that ref.
	body = get("/projects/proj/commits?owner=acme&repo=both&ref=origin/main")
	assertSelectedRef(t, body, "origin/main")
	if logRef != "origin/main" {
		t.Fatalf("explicit origin/main logged %q", logRef)
	}

	// Pagination keeps the resolved ref. Page size is 50; 51 rows make a next link.
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "log" {
			logRef = args[len(args)-1]
			var b strings.Builder
			for i := range 51 {
				fmt.Fprintf(&b, "%040x\x1fCommit %d\x1fAlice\x1fa@ex.com\x1f2026-07-20T12:00:00Z\n", i+1, i)
			}
			return []byte(b.String()), nil
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	}
	body = get(api + "&ref=origin/master")
	if logRef != "origin/master" {
		t.Fatalf("paged log ref=%q", logRef)
	}
	if !strings.Contains(body, `ref=origin%2Fmaster`) || !strings.Contains(body, `page=2`) {
		t.Fatalf("pager did not keep resolved ref: %s", body)
	}
}

func assertSelectedRef(t *testing.T, body, ref string) {
	t.Helper()
	want := `<option value="` + ref + `" selected>`
	if !strings.Contains(body, want) {
		t.Fatalf("selected ref %q missing in %s", ref, body)
	}
}

// assertRepoPickOmitsRef checks the rendered page drops #ref when #repo-pick
// changes, before data-autosubmit posts the filter. The select's own listener
// runs in the target phase; the document autosubmit listener runs on bubble.
func assertRepoPickOmitsRef(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, `id="repo-pick" name="repo_full" data-autosubmit`) {
		t.Fatal("repo pick must stay data-autosubmit")
	}
	i := strings.Index(body, `sel.addEventListener('change'`)
	if i < 0 {
		t.Fatal("repo-pick change listener missing")
	}
	rest := body[i:]
	j := strings.Index(rest, `form.addEventListener('submit'`)
	if j < 0 {
		t.Fatal("filter submit listener missing")
	}
	changeHandler := rest[:j]
	if !strings.Contains(changeHandler, `getElementById('ref')`) || !strings.Contains(changeHandler, `disabled=true`) {
		t.Fatalf("repo change must omit ref before submit: %s", changeHandler)
	}
	submit := rest[j:]
	if end := strings.Index(submit, `</script>`); end >= 0 {
		submit = submit[:end]
	}
	if strings.Contains(submit, "disabled") {
		t.Fatal("Apply submit must keep the chosen ref")
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
