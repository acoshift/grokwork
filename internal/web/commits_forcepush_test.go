package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
)

func TestCommitsForcePushMarkupWhenConfigured(t *testing.T) {
	srv, cfg, _ := testServer(t)
	proj, _ := cfg.ProjectPath("proj")
	if err := execGitInit(t, proj); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectGitHubRepos("proj", []config.GitHubRepoRef{{Owner: "acme", Repo: "app"}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging", "production"}, true); err != nil {
		t.Fatal(err)
	}

	body := getUnauthed(t, srv, "/projects/proj/commits?owner=acme&repo=app")
	for _, want := range []string{
		`id="commits-cherrypick"`,
		`action="/projects/proj/commits/force-push"`,
		`type="radio" name="sha"`,
		`data-confirm-title="Force-push to branch"`,
		`data-idle-label="Force-push"`,
		`>staging</option>`,
		`>Force-push</button>`,
		`Select one commit, then Confirm.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %q", want)
		}
	}
	if strings.Contains(body, `data-confirm-wait="Cherry-picking onto the target branch…"`) {
		t.Fatal("force-push list must not use cherry-pick confirm copy")
	}
	if strings.Contains(body, `type="checkbox" name="sha"`) {
		t.Fatal("force-push list must use radios for sha")
	}

	sha, err := gitworktreeOutput(t, proj, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	detail := getUnauthed(t, srv, "/projects/proj/commits/"+sha+"?owner=acme&repo=app")
	for _, want := range []string{
		`id="commit-cherrypick-actions"`,
		`action="/projects/proj/commits/force-push"`,
		`data-confirm-title="Force-push to branch"`,
		`>Force-push</button>`,
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
}

func TestCommitsForcePushForbiddenWithoutCanShip(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectSafeTeam("proj", true, "investigator", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/force-push", sid, csrf, url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {"abcd1234abcd1234abcd1234abcd1234abcd1234"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertAuditAction(t, srv, audit.ActionGitForcePush, false)
}

func TestCommitsForcePushCSRF(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/force-push", sid, "wrong", url.Values{
		"target": {"staging"}, "sha": {"abcd123"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCommitsForcePushRepoTraversal(t *testing.T) {
	srv, cfg, _ := authOnServer(t)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/projects/proj/commits/force-push", sid, csrf, url.Values{
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

func TestCommitsForcePushFF(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}

	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	})
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
	if strings.Contains(loc, "Force-pushed") {
		t.Fatalf("FF must not say Force-pushed: %s", loc)
	}
	got, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != sha {
		t.Fatalf("staging=%s want %s", got, sha)
	}
	assertAuditAction(t, srv, audit.ActionGitForcePush, true)
}

func TestCommitsForcePushRewind(t *testing.T) {
	srv, cfg, _ := testServer(t)
	remote, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	old, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	head, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {head},
	})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("ff: %s", loc)
	}
	w = postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {old},
	})
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("rewind err: %s", loc)
	}
	if !strings.Contains(loc, "Force-pushed") {
		t.Fatalf("want Force-pushed flash: %s", loc)
	}
	got, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(old) {
		t.Fatalf("staging=%s want %s", got, old)
	}
	_ = remote
}

func TestCommitsForcePushRefusesCherryPickRoute(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	before, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/cherrypick", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want refuse, got %s", loc)
	}
	got, err := gitworktreeOutput(t, main, "rev-parse", "--verify", "refs/remotes/origin/staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(before) {
		t.Fatal("remote moved")
	}
	assertAuditAction(t, srv, audit.ActionGitCherryPick, false)
}

func TestCommitsForcePushRefusedWhenCherryPickMode(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want refuse, got %s", loc)
	}
	assertAuditAction(t, srv, audit.ActionGitForcePush, false)
}

func TestCommitsForcePushTwoSHAsRefused(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha, sha},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("want refuse, got %s", loc)
	}
}

func TestCommitsForcePushOpenJobRedirects(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main, conflictSHA := setupWebConflictRepo(t)
	pointProjectAt(t, cfg, main)
	id := postConflictJob(t, srv, conflictSHA)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {conflictSHA},
	})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, id) {
		t.Fatalf("want redirect to existing job %s, got %s", id, loc)
	}
}

func TestWorkflowSavesForcePushMode(t *testing.T) {
	srv, cfg, _ := testServer(t)
	w := postUnauthed(t, srv, "/config/projects/cherry-pick-targets", url.Values{
		"name": {"proj"}, "cherryPickTargets": {"staging\nproduction"}, "forcePushTargets": {"1"},
	})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d loc=%s", w.Code, w.Header().Get("Location"))
	}
	if !cfg.ProjectForcePushTargets("proj") {
		t.Fatal("want force-push mode")
	}
	if got := cfg.ProjectCherryPickTargets("proj"); strings.Join(got, ",") != "staging,production" {
		t.Fatalf("targets: %v", got)
	}
}

func TestCommitsForcePushDoesNotTouchMainHEAD(t *testing.T) {
	srv, cfg, _ := testServer(t)
	_, main := setupWebCherryPickRepo(t)
	pointProjectAt(t, cfg, main)
	if err := cfg.SetProjectCherryPickConfig("proj", []string{"staging"}, true); err != nil {
		t.Fatal(err)
	}
	before, err := gitworktreeOutput(t, main, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := gitworktreeOutput(t, main, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w := postUnauthed(t, srv, "/projects/proj/commits/force-push", url.Values{
		"owner": {"acme"}, "repo": {"app"}, "target": {"staging"}, "sha": {sha},
	})
	if loc := w.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("%s", loc)
	}
	after, err := gitworktreeOutput(t, main, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatalf("HEAD moved %s -> %s", before, after)
	}
}
