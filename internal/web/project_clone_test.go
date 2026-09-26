package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
)

func TestAddProjectCloneAndPath(t *testing.T) {
	srv, cfg, _ := testServer(t)
	var calls int
	srv.cloneRepo = func(_ context.Context, remote, dest, branch string) error {
		calls++
		if remote != "acoshift/grokwork" || branch != "" {
			t.Fatalf("clone %q %q", remote, branch)
		}
		return os.Mkdir(filepath.Join(dest, ".git"), 0o755)
	}

	w := postProject(srv, url.Values{
		"source": {"clone"},
		"name":   {"app"},
		"remote": {"acoshift/grokwork"},
	})
	loc := redirectTarget(w)
	if !strings.HasPrefix(loc, "/config/projects/app?") || !strings.Contains(loc, "ok=") {
		t.Fatalf("status=%d loc=%q body=%s", w.Code, loc, w.Body.String())
	}
	got, ok := cfg.ProjectPath("app")
	want := filepath.Join(cfg.ReposRoot(), "app")
	if gr, err := filepath.EvalSymlinks(got); err == nil {
		got = gr
	}
	if wr, err := filepath.EvalSymlinks(want); err == nil {
		want = wr
	}
	if !ok || got != want {
		t.Fatalf("path=%q ok=%v want=%q", got, ok, want)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if _, err := os.Stat(filepath.Join(got, ".git")); err != nil {
		t.Fatal(err)
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range evs {
		if ev.Action == audit.ActionConfigAddProject && ev.OK && ev.Detail["via"] == "clone" && ev.Detail["remote"] == "acoshift/grokwork" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit=%+v", evs)
	}
}

func TestAddProjectCloneCaseDistinctName(t *testing.T) {
	srv, cfg, _ := testServer(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProject("App", elsewhere); err != nil {
		t.Fatal(err)
	}
	srv.cloneRepo = func(_ context.Context, _, dest, _ string) error {
		return os.Mkdir(filepath.Join(dest, ".git"), 0o755)
	}
	w := postProject(srv, url.Values{"source": {"clone"}, "name": {"app"}, "remote": {"acoshift/grokwork"}})
	if _, ok := cfg.ProjectPath("app"); !ok {
		t.Fatalf("loc=%q body=%s", redirectTarget(w), w.Body.String())
	}
	if got, _ := cfg.ProjectPath("App"); got != elsewhere {
		t.Fatalf("existing path changed: %q", got)
	}
}

func TestAddProjectCloneRejects(t *testing.T) {
	srv, cfg, _ := testServer(t)
	called := false
	srv.cloneRepo = func(context.Context, string, string, string) error {
		called = true
		return nil
	}
	w := postProject(srv, url.Values{
		"source": {"clone"},
		"name":   {"app"},
		"remote": {"file:///tmp/secret-repo"},
	})
	loc := redirectTarget(w)
	if !strings.HasPrefix(loc, "/config/projects/new?") || !strings.Contains(loc, "err=") {
		t.Fatalf("loc=%q", loc)
	}
	q, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if q.Query().Get("remote") != "" || strings.Contains(loc, "secret-repo") {
		t.Fatalf("loc leaked remote: %q", loc)
	}
	if _, ok := cfg.ProjectPath("app"); ok {
		t.Fatal("project was added")
	}
	if called {
		t.Fatal("cloner ran")
	}
	evs, err := srv.audit.ReadDay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for _, ev := range evs {
		if ev.Action == audit.ActionConfigAddProject && !ev.OK && ev.Detail["via"] == "clone" {
			failed = true
			if _, ok := ev.Detail["remote"]; ok {
				t.Fatalf("failed parse stored remote: %+v", ev.Detail)
			}
		}
	}
	if !failed {
		t.Fatalf("audit=%+v", evs)
	}

	w = postProject(srv, url.Values{"remote": {"acoshift/grokwork"}})
	if _, ok := cfg.ProjectPath("acoshift"); ok {
		t.Fatal("missing source added a project")
	}
	if !strings.Contains(redirectTarget(w), "/config/projects/new") {
		t.Fatalf("loc=%q", redirectTarget(w))
	}

	w = postProject(srv, url.Values{"name": {"nope"}, "path": {"relative/path"}})
	if !strings.Contains(redirectTarget(w), "/config/projects/new") {
		t.Fatalf("path error loc=%q", redirectTarget(w))
	}
}

func TestAddProjectCloneDoesNotReplaceExisting(t *testing.T) {
	srv, cfg, _ := testServer(t)
	keep := filepath.Join(t.TempDir(), "keep")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keep, "marker"), []byte("stay"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	srv.cloneRepo = func(_ context.Context, _, dest, _ string) error {
		called = true
		if err := os.Remove(dest); err != nil {
			return err
		}
		return os.Symlink(keep, dest)
	}
	w := postProject(srv, url.Values{"source": {"clone"}, "name": {"app"}, "remote": {"acoshift/grokwork"}})
	if !called || !strings.Contains(redirectTarget(w), "err=") {
		t.Fatalf("called=%v loc=%q", called, redirectTarget(w))
	}
	body, err := os.ReadFile(filepath.Join(keep, "marker"))
	if err != nil || string(body) != "stay" {
		t.Fatalf("symlink target damaged: %v %q", err, body)
	}
	if _, ok := cfg.ProjectPath("app"); ok {
		t.Fatal("project registered after clone error")
	}

	taken := filepath.Join(t.TempDir(), "taken")
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddProject("app", taken); err != nil {
		t.Fatal(err)
	}
	ran := false
	srv.cloneRepo = func(context.Context, string, string, string) error {
		ran = true
		return nil
	}
	postProject(srv, url.Values{"source": {"clone"}, "name": {"app"}, "remote": {"acoshift/grokwork"}})
	if ran {
		t.Fatal("cloner ran for an existing project")
	}
	if got, _ := cfg.ProjectPath("app"); got != taken {
		t.Fatalf("path changed to %q", got)
	}
}

func TestAddProjectCloneCleansUpWhenSaveFails(t *testing.T) {
	srv, cfg, _ := testServer(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.cloneRepo = func(_ context.Context, _, dest, _ string) error {
		if err := os.Mkdir(filepath.Join(dest, ".git"), 0o755); err != nil {
			return err
		}
		// Register the name while the clone lock is held so AddProject fails
		// after the checkout is renamed into place.
		return cfg.AddProject("app", other)
	}
	w := postProject(srv, url.Values{"source": {"clone"}, "name": {"app"}, "remote": {"acoshift/grokwork"}})
	if !strings.Contains(redirectTarget(w), "err=") {
		t.Fatalf("loc=%q", redirectTarget(w))
	}
	if got, _ := cfg.ProjectPath("app"); got != other {
		t.Fatalf("path=%q", got)
	}
	dest := filepath.Join(cfg.ReposRoot(), "app")
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest still present: %v", err)
	}
}

func TestAddProjectCloneRefusesWorktreeRoot(t *testing.T) {
	srv, cfg, _ := testServer(t)
	root := cfg.ReposRoot()
	if err := cfg.SetWorktreeDir(root); err != nil {
		t.Fatal(err)
	}
	ran := false
	srv.cloneRepo = func(context.Context, string, string, string) error {
		ran = true
		return nil
	}
	w := postProject(srv, url.Values{"source": {"clone"}, "name": {"app"}, "remote": {"acoshift/grokwork"}})
	if !strings.Contains(redirectTarget(w), "err=") || !strings.Contains(redirectTarget(w), "worktree") {
		t.Fatalf("loc=%q", redirectTarget(w))
	}
	if ran {
		t.Fatal("cloner ran")
	}
	if _, err := os.Lstat(filepath.Join(root, ".incoming")); !os.IsNotExist(err) {
		t.Fatalf("incoming created: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("dest created: %v", err)
	}
}

func TestAddProjectCloneNameRules(t *testing.T) {
	srv, _, _ := testServer(t)
	ran := false
	srv.cloneRepo = func(context.Context, string, string, string) error {
		ran = true
		return nil
	}
	for _, name := range []string{"../x", ".incoming", "app/ui", ".git", "-app"} {
		ran = false
		postProject(srv, url.Values{"source": {"clone"}, "name": {name}, "remote": {"acoshift/grokwork"}})
		if ran {
			t.Fatalf("cloner ran for %q", name)
		}
	}
}

func TestValidCloneProjectNameAllowsUnicode(t *testing.T) {
	if err := validCloneProjectName("โปรเจกต์"); err != nil {
		t.Fatal(err)
	}
	if err := validCloneProjectName("my app"); err == nil {
		t.Fatal("space accepted")
	}
}

func postProject(srv *Server, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/config/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func redirectTarget(w *httptest.ResponseRecorder) string {
	if loc := w.Header().Get("HX-Redirect"); loc != "" {
		return loc
	}
	return w.Header().Get("Location")
}
