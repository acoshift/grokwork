package gitworktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCloneRemote(t *testing.T) {
	ok := []struct {
		in, spec, scheme, host, audit string
		gh                            bool
		segs                          []string
	}{
		{in: "acoshift/grokwork", spec: "acoshift/grokwork", gh: true, segs: []string{"acoshift", "grokwork"}, audit: "acoshift/grokwork"},
		{in: "acoshift/grokwork.git", spec: "acoshift/grokwork", gh: true, segs: []string{"acoshift", "grokwork"}, audit: "acoshift/grokwork"},
		{in: "https://github.com/acoshift/grokwork", spec: "https://github.com/acoshift/grokwork", scheme: "https", host: "github.com", gh: true, segs: []string{"acoshift", "grokwork"}, audit: "github.com/acoshift/grokwork"},
		{in: "https://www.github.com/acoshift/grokwork.git", spec: "https://www.github.com/acoshift/grokwork", scheme: "https", host: "www.github.com", gh: true, segs: []string{"acoshift", "grokwork"}, audit: "github.com/acoshift/grokwork"},
		{in: "https://gitlab.com/group/sub/repo", spec: "https://gitlab.com/group/sub/repo", scheme: "https", host: "gitlab.com", segs: []string{"group", "sub", "repo"}, audit: "gitlab.com/group/sub/repo"},
		{in: "ssh://git@github.com/acoshift/grokwork.git", spec: "ssh://git@github.com/acoshift/grokwork", scheme: "ssh", host: "github.com", segs: []string{"acoshift", "grokwork"}, audit: "github.com/acoshift/grokwork"},
		{in: "git@gitlab.com:group/sub/repo.git", spec: "git@gitlab.com:group/sub/repo", scheme: "scp", host: "gitlab.com", segs: []string{"group", "sub", "repo"}, audit: "gitlab.com/group/sub/repo"},
	}
	for _, tc := range ok {
		got, err := ParseCloneRemote(tc.in)
		if err != nil {
			t.Fatalf("Parse %q: %v", tc.in, err)
		}
		if got.Spec != tc.spec || got.Scheme != tc.scheme || got.Host != tc.host || got.ViaGH != tc.gh || !sameSegments(got.Segments, tc.segs) || got.AuditLabel() != tc.audit {
			t.Fatalf("Parse %q = %+v", tc.in, got)
		}
	}

	bad := []string{
		"",
		"myrepo",
		"owner/repo/extra",
		"file:///tmp/repo",
		"ext::sh",
		"http://github.com/acoshift/grokwork",
		"-evil",
		"/tmp/repo",
		"../repo",
		"https://github.com/acoshift/../grokwork",
		"ssh://git@github.com/../../etc/passwd",
		"git@github.com:acoshift/../grokwork",
		"https://github.com/acoshift/grokwork?token=1",
		"https://github.com/acoshift/grokwork#frag",
		"owner/repo\nhttps://evil",
		"git@host:path with space",
	}
	for _, in := range bad {
		if _, err := ParseCloneRemote(in); err == nil {
			t.Fatalf("Parse %q succeeded", in)
		}
	}

	const token = "super-secret-token"
	_, err := ParseCloneRemote("https://user:" + token + "@github.com/acoshift/grokwork.git")
	if err == nil || err.Error() != EmbeddedCredentialMsg || strings.Contains(err.Error(), token) {
		t.Fatalf("credential error = %v", err)
	}
}

func TestOriginMatches(t *testing.T) {
	slug, err := ParseCloneRemote("acoshift/grokwork")
	if err != nil {
		t.Fatal(err)
	}
	gitlab, err := ParseCloneRemote("https://gitlab.com/group/sub/repo")
	if err != nil {
		t.Fatal(err)
	}
	www, err := ParseCloneRemote("https://www.github.com/acoshift/grokwork")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		req    CloneRemote
		origin string
		ok     bool
	}{
		{"ssh suffix", slug, "git@github.com:acoshift/grokwork.git", true},
		{"https case", slug, "https://github.com/Acoshift/Grokwork.git", true},
		{"trailing slash", slug, "https://github.com/acoshift/grokwork/", true},
		{"www host", www, "https://github.com/acoshift/grokwork.git", true},
		{"fork parent", slug, "https://github.com/other/grokwork.git", false},
		{"wrong host", www, "https://gitlab.com/acoshift/grokwork.git", false},
		{"subgroup", gitlab, "https://gitlab.com/group/sub/repo.git", true},
		{"other subgroup", gitlab, "https://gitlab.com/group/other/repo.git", false},
	}
	for _, tc := range cases {
		err := OriginMatches(tc.req, tc.origin)
		if tc.ok && err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: matched %q", tc.name, tc.origin)
		}
	}
	const token = "super-secret-token"
	err = OriginMatches(slug, "https://user:"+token+"@github.com/acoshift/grokwork.git")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("userinfo error = %v", err)
	}
}

func TestRejectCloneConfig(t *testing.T) {
	dir := initCloneRepo(t)
	cfg := filepath.Join(dir, ".git", "config")
	if err := rejectCloneConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"core.fsmonitor", "core.sshCommand", "core.hooksPath", "uploadpack.packObjectsHook"} {
		if err := gitConfigSet(t.Context(), cfg, key, "nope"); err != nil {
			t.Fatal(err)
		}
		err := rejectCloneConfig(t.Context(), cfg)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(key)) {
			t.Fatalf("%s: %v", key, err)
		}
		cmd := exec.CommandContext(t.Context(), "git", "config", "--file", cfg, "--unset-all", key)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unset %s: %v %s", key, err, out)
		}
	}
}

func TestFinishCloneCredentialHelper(t *testing.T) {
	dir := initCloneRepo(t)
	if err := runGitCmd(t, dir, "remote", "add", "origin", "https://github.com/acoshift/grokwork.git"); err != nil {
		t.Fatal(err)
	}
	req, err := ParseCloneRemote("acoshift/grokwork")
	if err != nil {
		t.Fatal(err)
	}
	if err := finishClone(t.Context(), dir, req); err != nil {
		t.Fatal(err)
	}
	got := credentialHelpers(t, filepath.Join(dir, ".git", "config"))
	if len(got) != 2 || got[0] != "" || got[1] != "!gh auth git-credential" {
		t.Fatalf("helpers=%#v", got)
	}

	sshDir := initCloneRepo(t)
	if err := runGitCmd(t, sshDir, "remote", "add", "origin", "git@github.com:acoshift/grokwork.git"); err != nil {
		t.Fatal(err)
	}
	if err := finishClone(t.Context(), sshDir, req); err != nil {
		t.Fatal(err)
	}
	if got := credentialHelpers(t, filepath.Join(sshDir, ".git", "config")); len(got) != 0 {
		t.Fatalf("ssh helpers=%#v", got)
	}
}

func TestCloneWithGitLocal(t *testing.T) {
	src := initCloneRepo(t)
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGitCmd(t, src, "add", "f"); err != nil {
		t.Fatal(err)
	}
	if err := runGitCmd(t, src, "commit", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	fileURL := "file://" + src
	dest := filepath.Join(t.TempDir(), "out")
	if err := cloneWithGit(t.Context(), fileURL, dest, "", "never"); err == nil {
		t.Fatal("file protocol allowed")
	}
	if err := cloneWithGit(t.Context(), fileURL, dest, "main", "always"); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(dest) {
		t.Fatal("not a repo")
	}
	head, err := gitOutput(t.Context(), dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != "main" {
		t.Fatalf("HEAD=%q", head)
	}

	busy := t.TempDir()
	if err := os.WriteFile(filepath.Join(busy, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cloneWithGit(t.Context(), fileURL, busy, "main", "always"); err == nil {
		t.Fatal("cloned into a non-empty directory")
	}
}

func TestCloneAtRejectsFlagBranch(t *testing.T) {
	err := CloneAt(t.Context(), "https://example.com/acme/app", filepath.Join(t.TempDir(), "out"), "-evil")
	if err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("err=%v", err)
	}
}

func initCloneRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := runGitCmd(t, dir, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := runGitCmd(t, dir, "config", "user.email", "t@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := runGitCmd(t, dir, "config", "user.name", "t"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runGitCmd(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func credentialHelpers(t *testing.T, cfg string) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "config", "--file", cfg, "--get-all", "credential.helper")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		t.Fatal(err)
	}
	raw := strings.TrimSuffix(string(out), "\n")
	if raw == "" && len(out) == 0 {
		return nil
	}
	return strings.Split(raw, "\n")
}
