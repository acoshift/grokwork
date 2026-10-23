package bot

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/agentapi"
	"github.com/acoshift/grokwork/internal/agentauth"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
)

func TestNormalizeDispatchInputs(t *testing.T) {
	spec := ghpr.DispatchSpec{Dispatchable: true, Inputs: []ghpr.DispatchInput{
		{Name: "env", Required: true, Type: "choice", Options: []string{"stag", "prod"}},
		{Name: "dry", Type: "boolean", Default: "true"},
		{Name: "note", Type: "string"},
		{Name: "flag", Type: "boolean"},
	}}
	got, err := normalizeDispatchInputs(spec, map[string]string{
		"env": "prod", "note": " hi ", "flag": "on", "extra": "nope",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["env"] != "prod" || got["note"] != "hi" || got["dry"] != "true" || got["flag"] != "true" {
		t.Fatalf("%v", got)
	}
	if _, ok := got["extra"]; ok {
		t.Fatalf("unknown input kept: %v", got)
	}

	if _, err := normalizeDispatchInputs(spec, map[string]string{"note": "x"}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing required: %v", err)
	}
	if _, err := normalizeDispatchInputs(spec, map[string]string{"env": "dev"}); err == nil || !strings.Contains(err.Error(), "one of") {
		t.Fatalf("bad choice: %v", err)
	}
	off, err := normalizeDispatchInputs(spec, map[string]string{"env": "stag", "flag": "no"})
	if err != nil {
		t.Fatal(err)
	}
	if off["flag"] != "false" {
		t.Fatalf("flag=%q", off["flag"])
	}
}

func TestOpsCherryPickUnlistedTargetBeforeGit(t *testing.T) {
	dir := t.TempDir()
	b := &Bot{cfg: cherryCfg(dir, []string{"staging"}, false)}
	_, err := b.OpsCherryPick(t.Context(), agentauth.Cred{ActorID: "u", Project: "app"}, agentapi.OpsCherryPickRequest{
		Target: "main", SHAs: []string{"abc123"},
	})
	if err == nil || !strings.Contains(err.Error(), "allow") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("path leaked: %v", err)
	}
}

func TestOpsCherryPickForcePushRefused(t *testing.T) {
	dir := t.TempDir()
	b := &Bot{cfg: cherryCfg(dir, []string{"staging"}, true)}
	_, err := b.OpsCherryPick(t.Context(), agentauth.Cred{ActorID: "u", Project: "app"}, agentapi.OpsCherryPickRequest{
		Target: "staging", SHAs: []string{"abc123"},
	})
	if err == nil || !strings.Contains(err.Error(), "force-push") || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "use Force-push") || strings.Contains(err.Error(), dir) {
		t.Fatalf("bad error: %v", err)
	}
}

func TestOpsCherryPickRefusesPrimary(t *testing.T) {
	dir := initEmptyRepo(t)
	branch := gitBranch(t, dir)
	b := &Bot{cfg: cherryCfg(dir, []string{branch, "staging"}, false)}
	b.cfg.DataDir = t.TempDir()
	_, err := b.OpsCherryPick(t.Context(), agentauth.Cred{ActorID: "u", Project: "app"}, agentapi.OpsCherryPickRequest{
		Target: branch, SHAs: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("path leaked: %v", err)
	}
}

func TestOpsLogRejectsOptionRef(t *testing.T) {
	b := &Bot{cfg: cherryCfg(t.TempDir(), nil, false)}
	cred := agentauth.Cred{ActorID: "u", Project: "app"}
	for _, ref := range []string{"--output=/tmp/x", "/etc/passwd", "main --all"} {
		_, err := b.OpsLog(t.Context(), cred, ref, "", "", 0)
		if err == nil || !strings.Contains(err.Error(), "invalid ref") {
			t.Fatalf("ref %q: %v", ref, err)
		}
	}
	cfg := cherryCfg(t.TempDir(), nil, false)
	cfg.WebAuth = &config.WebAuthConfig{
		Enabled:  true,
		Features: config.WebAuthFeatures{GitHubWrites: true},
	}
	b = &Bot{cfg: cfg}
	_, err := b.OpsDispatch(t.Context(), cred, agentapi.OpsDispatchRequest{Workflow: "ci.yml", Ref: "--repo"})
	if err == nil || !strings.Contains(err.Error(), "invalid ref") {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestScrubOpsReplacesRoots(t *testing.T) {
	dir := t.TempDir()
	b := &Bot{cfg: &config.Config{
		DataDir:  dir + "/data",
		Projects: config.ProjectsMap{"app": {Path: dir + "/proj"}},
	}}
	err := b.scrubProject("app", errors.New("boom at "+dir+"/proj/x and "+dir+"/data/y"), dir+"/extra")
	if err == nil || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "[path]") {
		t.Fatalf("%v", err)
	}
}

func cherryCfg(path string, targets []string, force bool) *config.Config {
	pc := config.ProjectConfig{Path: path, CherryPickTargets: targets}
	if force {
		pc.ForcePushTargets = new(true)
	}
	return &config.Config{Projects: config.ProjectsMap{"app": pc}}
}

func initEmptyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	return dir
}

func gitBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		t.Fatalf("branch=%q", branch)
	}
	return branch
}
