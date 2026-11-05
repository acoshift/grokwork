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
	"sync"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
)

func TestMergeFetchesCheckoutInBackground(t *testing.T) {
	srv, cfg, _ := writeEnabledServer(t)
	remote, seed := testRemotePair(t)
	clone := t.TempDir()
	gitRun(t, clone, "clone", remote, ".")
	pointMergeProjectAt(cfg, clone)

	before := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main")
	advanceRemote(t, seed)
	want := gitOut(t, seed, "rev-parse", "HEAD")
	if want == before {
		t.Fatal("remote did not advance")
	}

	postMerge(t, srv, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main")
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("origin/main=%s want %s (idle fetch is off; merge should have fetched)", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMergePreflightDoesNotFetch(t *testing.T) {
	srv, cfg, _ := writeEnabledServer(t)
	remote, seed := testRemotePair(t)
	clone := t.TempDir()
	gitRun(t, clone, "clone", remote, ".")
	pointMergeProjectAt(cfg, clone)

	before := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main")
	advanceRemote(t, seed)

	var mu sync.Mutex
	srv.ghRunner = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{
				"number":9,"url":"https://github.com/acme/app/pull/9","title":"T","state":"OPEN",
				"isDraft":false,"reviewDecision":"","headRefOid":"a","headRefName":"f",
				"baseRefName":"main","body":"","mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","author":{"login":"z"},
				"additions":0,"deletions":0,"changedFiles":0,"comments":[]
			}`), nil
		case strings.HasPrefix(joined, "pr checks"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "pr merge"):
			t.Fatal("preflight conflict must not merge")
		}
		return nil, nil
	}

	postMerge(t, srv, func(code int, loc string) {
		if code != http.StatusFound && code != http.StatusSeeOther {
			t.Fatalf("status=%d", code)
		}
		if !strings.Contains(loc, "err=") {
			t.Fatalf("Location=%q want err", loc)
		}
	})
	time.Sleep(500 * time.Millisecond)
	got := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main")
	if got != before {
		t.Fatalf("failed merge fetched anyway: origin/main=%s was %s", got, before)
	}
}

func pointMergeProjectAt(cfg *config.Config, clone string) {
	pc := cfg.Projects["proj"]
	pc.Path = clone
	pc.RepoFetchIntervalMinutes = new(0)
	cfg.Projects["proj"] = pc
}

func postMerge(t *testing.T, srv *Server, check func(code int, loc string)) {
	t.Helper()
	sid, csrf, err := srv.LoginAs("admin-1", "A", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"project": {"proj"}, "csrf": {csrf}, "method": {"squash"}}
	req := httptest.NewRequest(http.MethodPost, "/prs/acme/app/9/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if check != nil {
		check(w.Code, w.Header().Get("Location"))
		return
	}
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Header().Get("Location"), "err=") {
		t.Fatalf("Location=%q", w.Header().Get("Location"))
	}
}

func advanceRemote(t *testing.T, seed string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "a.txt")
	gitRun(t, seed, "commit", "-m", "v2")
	gitRun(t, seed, "push", "origin", "main")
}

func testRemotePair(t *testing.T) (remote, seed string) {
	t.Helper()
	remote = t.TempDir()
	gitRun(t, remote, "init", "--bare")
	seed = t.TempDir()
	gitRun(t, seed, "init")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "a.txt")
	gitRun(t, seed, "commit", "-m", "v1")
	gitRun(t, seed, "branch", "-M", "main")
	gitRun(t, seed, "remote", "add", "origin", remote)
	gitRun(t, seed, "push", "-u", "origin", "main")
	gitRun(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote, seed
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
