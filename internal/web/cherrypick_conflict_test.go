package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
)

func TestCherryPickConflictRedirectsToResolvePage(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)

	form := url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {conflictSHA},
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
	if !strings.Contains(loc, "/projects/proj/cherrypick/cp_") {
		t.Fatalf("want conflict page, got %s", loc)
	}

	pageReq := httptest.NewRequest(http.MethodGet, loc, nil)
	pageW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(pageW, pageReq)
	if pageW.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", pageW.Code, pageW.Body.String())
	}
	body := pageW.Body.String()
	for _, want := range []string{
		`id="page-cherrypick-conflict"`,
		`data-grok-stream`,
		`Suggest with Grok`,
		`Apply &amp; continue`,
		`>Abort</button>`,
		`Ours (target)`,
		`Theirs (picked commit)`,
		`>Save file</button>`,
		`form="cp-file-0"`,
		`.form-actions > form {`,
		`name="content"`,
		`README`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}

	commits := getUnauthed(t, srv, "/projects/proj/commits?owner=acme&repo=app")
	if !strings.Contains(commits, "/projects/proj/cherrypick/cp_") {
		t.Fatal("commits list must link the parked job")
	}
}

func TestCherryPickConflictContinueAndAbort(t *testing.T) {
	srv, cfg, _ := testServer(t)
	remote, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	before := originSHAWeb(t, main, "staging")

	id := postConflictJob(t, srv, conflictSHA)
	base := "/projects/proj/cherrypick/" + id

	w := postUnauthed(t, srv, base+"/file", url.Values{
		"path": {"README"}, "content": {"mainline\n"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("save status=%d loc=%s", w.Code, w.Header().Get("Location"))
	}

	w = postUnauthed(t, srv, base+"/continue", nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("continue status=%d loc=%s body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("continue err: %s", loc)
	}
	if !strings.Contains(loc, "/projects/proj/commits") || !strings.Contains(loc, "ok=") {
		t.Fatalf("want commits ok flash: %s", loc)
	}
	got := originSHAWeb(t, main, "staging")
	if got == before {
		t.Fatal("continue must push")
	}
	blob, err := gitworktreeOutput(t, main, "show", "origin/staging:README")
	if err != nil || strings.TrimSpace(blob) != "mainline" {
		t.Fatalf("README=%q err=%v", blob, err)
	}

	// A second conflict, then abort — remote must stay put.
	side := filepath.Join(t.TempDir(), "side")
	runGitWeb(t, t.TempDir(), "clone", remote, side)
	runGitWeb(t, side, "config", "user.name", "test")
	runGitWeb(t, side, "config", "user.email", "test@example.com")
	runGitWeb(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "README"), []byte("staging-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWeb(t, side, "add", "README")
	runGitWeb(t, side, "commit", "-m", "staging 2")
	runGitWeb(t, side, "push", "origin", "staging")
	runGitWeb(t, main, "fetch", "origin")
	if err := os.WriteFile(filepath.Join(main, "README"), []byte("main-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWeb(t, main, "add", "README")
	runGitWeb(t, main, "commit", "-m", "main 2")
	runGitWeb(t, main, "push", "origin", "main")
	sha2, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	before2 := originSHAWeb(t, main, "staging")
	id2 := postConflictJob(t, srv, sha2)
	w = postUnauthed(t, srv, "/projects/proj/cherrypick/"+id2+"/abort", nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("abort status=%d", w.Code)
	}
	if originSHAWeb(t, main, "staging") != before2 {
		t.Fatal("abort moved remote")
	}
}

func TestCherryPickContinueWithMarkersStaysOpen(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	before := originSHAWeb(t, main, "staging")
	id := postConflictJob(t, srv, conflictSHA)
	base := "/projects/proj/cherrypick/" + id

	w := postUnauthed(t, srv, base+"/continue", nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("continue status=%d loc=%s body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/projects/proj/cherrypick/"+id) || !strings.Contains(loc, "err=") {
		t.Fatalf("want resolve-page err flash, got %s", loc)
	}
	j, err := gitworktree.LoadJob(cfg.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Open() {
		t.Fatalf("job status=%s, want conflict so Save/Apply still work", j.Status)
	}
	if originSHAWeb(t, main, "staging") != before {
		t.Fatal("continue-with-markers must not push")
	}

	w = postUnauthed(t, srv, base+"/file", url.Values{
		"path": {"README"}, "content": {"mainline\n"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("save status=%d loc=%s", w.Code, w.Header().Get("Location"))
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("save after leftover continue: %s", loc)
	}

	w = postUnauthed(t, srv, base+"/continue", nil)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("apply status=%d loc=%s body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	loc = w.Header().Get("Location")
	if strings.Contains(loc, "err=") || !strings.Contains(loc, "/projects/proj/commits") {
		t.Fatalf("want commits ok after save+apply, got %s", loc)
	}
	got := originSHAWeb(t, main, "staging")
	if got == before {
		t.Fatal("save+apply must push")
	}
	blob, err := gitworktreeOutput(t, main, "show", "origin/staging:README")
	if err != nil || strings.TrimSpace(blob) != "mainline" {
		t.Fatalf("README=%q err=%v", blob, err)
	}
}

func TestCherryPickSuggestNoSession(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	id := postConflictJob(t, srv, conflictSHA)
	beforeSessions := srv.sessions.Count()
	histBefore, err := srv.history.List()
	if err != nil {
		t.Fatal(err)
	}
	beforeRemote := originSHAWeb(t, main, "staging")

	called := false
	srv.suggestConflict = func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, files []string, target, sha string, hooks *grokrun.SuggestStreamHooks) (string, error) {
		called = true
		if cwd == "" {
			t.Fatal("empty cwd")
		}
		if timeout != 3*time.Minute {
			t.Fatalf("timeout=%s", timeout)
		}
		if !strings.Contains(strings.Join(files, ","), "README") {
			t.Fatalf("files=%v", files)
		}
		if hooks != nil {
			if hooks.OnActivity != nil {
				hooks.OnActivity("write_file: README")
			}
			if hooks.OnTextDelta != nil {
				hooks.OnTextDelta("kept the picked change")
			}
		}
		if err := gitworktree.WriteWorkingFile(cwd, "README", []byte("mainline\n")); err != nil {
			t.Fatal(err)
		}
		return "kept the picked change", nil
	}

	form := url.Values{}
	req := httptest.NewRequest(http.MethodPost, "/projects/proj/cherrypick/"+id+"/suggest", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("suggest status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}
	if !called {
		t.Fatal("suggestConflict not called")
	}
	body := w.Body.String()
	for _, want := range []string{
		"event: status",
		"event: activity",
		"event: text",
		"event: result",
		"event: done",
		`"ok":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE missing %q:\n%s", want, body)
		}
	}
	if srv.sessions.Count() != beforeSessions {
		t.Fatalf("session store grew: %d → %d", beforeSessions, srv.sessions.Count())
	}
	histAfter, err := srv.history.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(histAfter) != len(histBefore) {
		t.Fatal("history grew")
	}
	if originSHAWeb(t, main, "staging") != beforeRemote {
		t.Fatal("suggest must not push")
	}
	j, err := gitworktree.LoadJob(cfg.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitworktree.ReadWorkingFile(j.Checkout, "README", 4096)
	if err != nil || strings.TrimSpace(string(got)) != "mainline" {
		t.Fatalf("working tree=%q err=%v", got, err)
	}
	if !gitworktree.SequencerLive(t.Context(), j.Checkout) {
		t.Fatal("suggest must not continue the sequencer")
	}
}

func TestCherryPickConflictForbiddenWithoutCanShip(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	id := seedFakeConflictJob(t, cfg)
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	base := "/projects/proj/cherrypick/" + id
	for _, path := range []string{base + "/continue", base + "/abort", base + "/file", base + "/suggest"} {
		w := postFix(t, srv, path, sid, csrf, url.Values{"path": {"README"}, "content": {"x"}})
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, base, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Apply &amp; continue") {
		t.Fatal("investigator must not see Apply")
	}
}

func TestCherryPickConflictCSRF(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}
	id := seedFakeConflictJob(t, cfg)
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/cherrypick/"+id+"/continue", sid, "wrong", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCherryPickConflictPathEscape(t *testing.T) {
	srv, cfg, _ := testServer(t)
	id := seedFakeConflictJob(t, cfg)
	w := postUnauthed(t, srv, "/projects/proj/cherrypick/"+id+"/file", url.Values{
		"path": {"../secret"}, "content": {"nope"},
	})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d loc=%s body=%s", w.Code, loc, w.Body.String())
	}
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want err flash, got %s", loc)
	}
}

func TestCherryPickOpenJobRedirects(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	id := postConflictJob(t, srv, conflictSHA)
	form := url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {conflictSHA},
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/cherrypick", form)
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, id) {
		t.Fatalf("want redirect to existing job %s, got %s", id, loc)
	}
}

func postConflictJob(t *testing.T, srv *Server, sha string) string {
	t.Helper()
	w := postUnauthed(t, srv, "/projects/proj/commits/cherrypick", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	})
	loc := w.Header().Get("Location")
	const prefix = "/projects/proj/cherrypick/"
	idx := strings.Index(loc, prefix)
	if idx < 0 {
		t.Fatalf("want conflict job redirect, got %s (status=%d body=%s)", loc, w.Code, w.Body.String())
	}
	id := strings.TrimPrefix(loc[idx:], prefix)
	if q := strings.IndexByte(id, '?'); q >= 0 {
		id = id[:q]
	}
	if !gitworktree.ValidJobID(id) {
		t.Fatalf("job id %q", id)
	}
	return id
}

func postUnauthed(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func seedFakeConflictJob(t *testing.T, cfg *config.Config) string {
	t.Helper()
	id := "cp_" + "dddddddddddddddddddddddddddddddd"
	j := gitworktree.Job{
		ID:       id,
		Project:  "proj",
		RepoPath: cfg.Projects["proj"].Path,
		Checkout: filepath.Join(t.TempDir(), "co"),
		Target:   "staging",
		Files:    []string{"README"},
		Status:   gitworktree.JobStatusConflict,
		Current:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := gitworktree.SaveJob(cfg.DataDir, j); err != nil {
		t.Fatal(err)
	}
	return id
}

func pointProjectAt(t *testing.T, cfg *config.Config, main string) {
	t.Helper()
	pc := cfg.Projects["proj"]
	pc.Path = main
	cfg.Projects["proj"] = pc
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickTargets("proj", []string{"staging"}); err != nil {
		t.Fatal(err)
	}
}

func setupWebConflictRepo(t *testing.T) (remote, main, conflictSHA string) {
	t.Helper()
	remote, main = setupWebCherryPickRepo(t)
	side := filepath.Join(t.TempDir(), "side")
	runGitWeb(t, t.TempDir(), "clone", remote, side)
	runGitWeb(t, side, "config", "user.name", "test")
	runGitWeb(t, side, "config", "user.email", "test@example.com")
	runGitWeb(t, side, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(side, "README"), []byte("staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWeb(t, side, "add", "README")
	runGitWeb(t, side, "commit", "-m", "staging readme")
	runGitWeb(t, side, "push", "origin", "staging")
	runGitWeb(t, main, "fetch", "origin")
	runGitWeb(t, main, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(main, "README"), []byte("mainline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWeb(t, main, "add", "README")
	runGitWeb(t, main, "commit", "-m", "main readme")
	runGitWeb(t, main, "push", "origin", "main")
	var err error
	conflictSHA, err = gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return remote, main, conflictSHA
}

func originSHAWeb(t *testing.T, repo, branch string) string {
	t.Helper()
	runGitWeb(t, repo, "fetch", "origin")
	sha, err := gitworktreeOutput(t, repo, "rev-parse", "--verify", "origin/"+branch+"^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func runGitWeb(t *testing.T, dir string, args ...string) {
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

func TestCherryPickConflictAuditSuggest(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	id := postConflictJob(t, srv, conflictSHA)
	srv.suggestConflict = func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, files []string, target, sha string, hooks *grokrun.SuggestStreamHooks) (string, error) {
		return "ok", nil
	}
	w := postUnauthed(t, srv, "/projects/proj/cherrypick/"+id+"/suggest", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	assertAuditAction(t, srv, audit.ActionGitCherryPickSuggest, true)
}
