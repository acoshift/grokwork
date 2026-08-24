package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

func TestCommitsCherryPickMarkupWhenConfigured(t *testing.T) {
	srv, cfg, _ := testServer(t)
	proj, _ := cfg.ProjectPath("proj")
	if err := execGitInit(t, proj); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging", "production"}); err != nil {
		t.Fatal(err)
	}

	body := getUnauthed(t, srv, "/projects/proj/commits?owner=acme&repo=app")
	for _, want := range []string{
		`id="commits-cherrypick"`,
		`name="sha"`,
		`name="target"`,
		`data-confirm-select="target"`,
		`>staging</option>`,
		`>production</option>`,
		`id="commits-table"`,
		`id="btn-commits-cherrypick"`,
		`id="commits-cherrypick-target" hidden`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %q", want)
		}
	}
	if strings.Contains(body, `form="commits-cherrypick" disabled`) {
		t.Fatal("list cherry-pick must be clickable when commits exist")
	}

	// Detail rail: need a real SHA from the repo.
	sha, err := gitworktreeOutput(t, proj, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	detail := getUnauthed(t, srv, "/projects/proj/commits/"+sha+"?owner=acme&repo=app")
	for _, want := range []string{
		`id="commit-cherrypick-actions"`,
		`name="target"`,
		`data-confirm-select="target"`,
		`name="sha"`,
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
}

func TestCommitsCherryPickForbiddenWithoutCanShip(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/cherrypick", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {"abcd1234abcd1234abcd1234abcd1234abcd1234"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertAuditAction(t, srv, audit.ActionGitCherryPick, false)
}

func TestCommitsCherryPickCSRF(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/cherrypick", sid, "wrong", url.Values{
		"target": {"staging"}, "sha": {"abcd123"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCommitsCherryPickRepoTraversal(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/cherrypick", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"../secret"}, "target": {"staging"}, "sha": {"abcd123"},
	})
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "secret") {
		t.Fatal("traversal leaked")
	}
	loc := w.Header().Get("Location")
	if w.Code != http.StatusForbidden && !strings.Contains(loc, "err=") && w.Code != http.StatusFound {
		t.Fatalf("status=%d loc=%s body=%s", w.Code, loc, w.Body.String())
	}
}

func TestCommitsCherryPickPushesToTarget(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	// Replace empty proj dir with the real clone by pointing config at main.
	pc := cfg.Projects["proj"]
	pc.Path = main
	cfg.Projects["proj"] = pc
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}

	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/commits/cherrypick", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d loc=%s body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("redirect err: %s", loc)
	}
	if !strings.Contains(loc, "ok=") {
		t.Fatalf("want ok flash: %s", loc)
	}
	got, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != sha {
		t.Fatalf("staging=%s want %s", got, sha)
	}
}

func getUnauthed(t *testing.T, srv *Server, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func gitworktreeOutput(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func setupWebCherryPickRepo(t *testing.T) (remote, main string) {
	t.Helper()
	remote = filepath.Join(t.TempDir(), "remote.git")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(t.TempDir(), "init", "--bare", remote)
	seed := t.TempDir()
	run(seed, "init")
	run(seed, "branch", "-M", "main")
	run(seed, "config", "user.name", "test")
	run(seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "README")
	run(seed, "commit", "-m", "init")
	run(seed, "remote", "add", "origin", remote)
	run(seed, "push", "-u", "origin", "main")
	run(seed, "branch", "staging")
	run(seed, "push", "-u", "origin", "staging")
	run(seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "b.txt")
	run(seed, "commit", "-m", "b")
	run(seed, "push", "origin", "main")

	main = filepath.Join(t.TempDir(), "main")
	cmd := exec.Command("git", "clone", remote, main)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	run(main, "checkout", "main")
	run(main, "config", "user.name", "test")
	run(main, "config", "user.email", "test@example.com")
	return remote, main
}
