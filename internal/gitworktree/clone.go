package gitworktree

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// EmbeddedCredentialMsg is the only text returned when a remote URL carries
// credentials. Callers must not echo the URL: it is about to be a query
// parameter and an audit row.
const EmbeddedCredentialMsg = "URLs with embedded credentials are rejected"

// CloneRemote is a repository a project add is allowed to clone.
// Host is empty for an owner/repo slug. Segments are the path with one
// trailing ".git" stripped off the last element.
type CloneRemote struct {
	Spec     string
	Scheme   string // "https", "ssh", "scp", or "" for a slug
	Host     string
	Segments []string
	ViaGH    bool
}

// AuditLabel is host/path or owner/repo, with no userinfo and no query.
func (r CloneRemote) AuditLabel() string {
	path := strings.Join(r.Segments, "/")
	if r.Host == "" {
		return path
	}
	return foldGitHost(r.Host) + "/" + path
}

// ParseCloneRemote accepts a GitHub owner/repo slug, https, ssh://, or
// git@host:path. file, http, ext, local paths, and embedded credentials are
// rejected. The returned Spec is a cleaned URL or slug, never the raw input.
func ParseCloneRemote(raw string) (CloneRemote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CloneRemote{}, fmt.Errorf("repository is required")
	}
	if strings.ContainsAny(raw, "\r\n \t") || strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	if strings.HasPrefix(raw, "-") {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	switch {
	case strings.Contains(raw, "://"):
		return parseCloneURL(raw)
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		return parseCloneSCP(raw)
	default:
		// A leading slash or dot is a local path. gh would otherwise treat
		// "/tmp/repo" as the slug "tmp/repo".
		if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") {
			return CloneRemote{}, fmt.Errorf("invalid repository URL")
		}
		return parseCloneSlug(raw)
	}
}

func parseCloneSlug(raw string) (CloneRemote, error) {
	segs, err := cleanGitPath(raw)
	if err != nil || len(segs) != 2 || !gitSlugPart(segs[0]) || !gitSlugPart(segs[1]) {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	return CloneRemote{
		Spec:     strings.Join(segs, "/"),
		Segments: segs,
		ViaGH:    true,
	}, nil
}

func gitSlugPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func parseCloneURL(raw string) (CloneRemote, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https", "ssh":
	default:
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass || scheme == "https" {
			return CloneRemote{}, fmt.Errorf("%s", EmbeddedCredentialMsg)
		}
	}
	segs, err := cleanGitPath(u.Path)
	if err != nil {
		return CloneRemote{}, err
	}
	host := u.Host
	remote := CloneRemote{
		Scheme:   scheme,
		Host:     host,
		Segments: segs,
		ViaGH:    scheme == "https" && foldGitHost(host) == "github.com",
	}
	rebuilt := url.URL{Scheme: scheme, Host: host, Path: "/" + strings.Join(segs, "/")}
	if scheme == "ssh" && u.User != nil {
		rebuilt.User = url.User(u.User.Username())
	}
	remote.Spec = rebuilt.String()
	return remote, nil
}

func parseCloneSCP(raw string) (CloneRemote, error) {
	user, rest, ok := strings.Cut(raw, "@")
	if !ok || user == "" || strings.Contains(user, ":") {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	host, path, ok := strings.Cut(rest, ":")
	if !ok || host == "" || strings.Contains(host, "/") {
		return CloneRemote{}, fmt.Errorf("invalid repository URL")
	}
	segs, err := cleanGitPath(path)
	if err != nil {
		return CloneRemote{}, err
	}
	return CloneRemote{
		Spec:     user + "@" + host + ":" + strings.Join(segs, "/"),
		Scheme:   "scp",
		Host:     host,
		Segments: segs,
	}, nil
}

func cleanGitPath(p string) ([]string, error) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" || strings.ContainsRune(p, '\\') {
		return nil, fmt.Errorf("invalid repository URL")
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, " \t") {
			return nil, fmt.Errorf("invalid repository URL")
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("invalid repository URL")
			}
		}
		if i == len(segs)-1 {
			seg = strings.TrimSuffix(seg, ".git")
			if seg == "" || seg == "." || seg == ".." {
				return nil, fmt.Errorf("invalid repository URL")
			}
		}
		segs[i] = seg
	}
	return segs, nil
}

func foldGitHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	name := host
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		name, port = h, p
	}
	if name == "www.github.com" {
		name = "github.com"
	}
	if port != "" {
		return net.JoinHostPort(name, port)
	}
	return name
}

// OriginMatches reports whether the cloned origin URL is the repository that
// was requested. A slug matches any host with that exact path, so a GitHub
// Enterprise checkout still passes. Any other request must match host and the
// full path, not just the last two segments.
func OriginMatches(req CloneRemote, origin string) error {
	got, err := ParseCloneRemote(strings.TrimSpace(origin))
	if err != nil {
		return err
	}
	return originMatchesParsed(req, got)
}

func originMatchesParsed(req, got CloneRemote) error {
	if !sameSegments(req.Segments, got.Segments) {
		return fmt.Errorf("clone origin does not match the requested repository")
	}
	if req.Host == "" {
		if got.Host == "" {
			return fmt.Errorf("clone origin has no host")
		}
		return nil
	}
	if foldGitHost(req.Host) != foldGitHost(got.Host) {
		return fmt.Errorf("clone origin does not match the requested repository")
	}
	return nil
}

func sameSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// CloneAt clones remote into dest. dest may be absent or an empty directory.
// On failure the destination is left for the caller to remove; this function
// does not delete it.
func CloneAt(ctx context.Context, remote, dest, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch != "" && (!validAdoptSyntax(branch) || !validGitRefName(ctx, branch)) {
		return fmt.Errorf("invalid branch %q", branch)
	}
	parsed, err := ParseCloneRemote(remote)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(dest); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("clone destination is not an empty directory")
		}
		entries, err := os.ReadDir(dest)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("clone destination is not an empty directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if parsed.ViaGH {
		err = cloneWithGH(ctx, parsed.Spec, dest, branch)
	} else {
		err = cloneWithGit(ctx, parsed.Spec, dest, branch, "never")
	}
	if err != nil {
		return err
	}
	return finishClone(ctx, dest, parsed)
}

func cloneWithGH(ctx context.Context, spec, dest, branch string) error {
	args := []string{
		"repo", "clone", "--no-upstream", spec, dest, "--",
		"--no-local",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"--no-recurse-submodules",
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	return runCloneCmd(ctx, "gh", args...)
}

func cloneWithGit(ctx context.Context, remote, dest, branch, fileAllow string) error {
	if fileAllow == "" {
		fileAllow = "never"
	}
	args := []string{
		"-c", "protocol.file.allow=" + fileAllow,
		"-c", "protocol.ext.allow=never",
		"clone", "--no-recurse-submodules", "--no-local",
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", remote, dest)
	return runCloneCmd(ctx, "git", args...)
}

func runCloneCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = cloneEnviron()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		msg := scrubCloneText(buf.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}

func cloneEnviron() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_TERMINAL_PROMPT=") || strings.HasPrefix(e, "GH_PROMPT_DISABLED=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
}

// ScrubCloneText strips URL userinfo and keeps the tail of a long git/gh
// message. The fatal line is at the end; progress is not.
func ScrubCloneText(s string) string {
	return scrubCloneText(s)
}

func scrubCloneText(s string) string {
	s = userinfoRE.ReplaceAllString(s, "${1}")
	s = strings.TrimSpace(s)
	const maxRunes = 400
	r := []rune(s)
	if len(r) > maxRunes {
		s = string(r[len(r)-maxRunes:])
	}
	return s
}

// Scheme, then userinfo, then @. The replacement keeps the scheme.
var userinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s]+@`)

var forbiddenCloneConfig = map[string]struct{}{
	"core.fsmonitor":             {},
	"core.sshcommand":            {},
	"core.hookspath":             {},
	"uploadpack.packobjectshook": {},
}

func finishClone(ctx context.Context, dir string, req CloneRemote) error {
	cfg := filepath.Join(dir, ".git", "config")
	if err := rejectCloneConfig(ctx, cfg); err != nil {
		return err
	}
	origin, err := gitConfigGet(ctx, cfg, "remote.origin.url")
	if err != nil || origin == "" {
		return fmt.Errorf("clone has no origin")
	}
	got, err := ParseCloneRemote(origin)
	if err != nil {
		return err
	}
	if err := originMatchesParsed(req, got); err != nil {
		return err
	}
	if req.ViaGH && got.Scheme == "https" {
		// The empty helper is first on purpose: it resets the list so a
		// global helper does not also run, and no token is written down.
		if err := gitConfigSet(ctx, cfg, "credential.helper", ""); err != nil {
			return err
		}
		if err := gitConfigAdd(ctx, cfg, "credential.helper", "!gh auth git-credential"); err != nil {
			return err
		}
	}
	return nil
}

func rejectCloneConfig(ctx context.Context, configPath string) error {
	out, err := gitConfigOutput(ctx, configPath, "--list")
	if err != nil {
		return fmt.Errorf("read clone config: %s", scrubCloneText(err.Error()))
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if _, bad := forbiddenCloneConfig[key]; bad {
			return fmt.Errorf("refusing checkout with %s set", key)
		}
	}
	return nil
}

func gitConfigGet(ctx context.Context, file, key string) (string, error) {
	out, err := gitConfigOutput(ctx, file, "--get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func gitConfigSet(ctx context.Context, file, key, value string) error {
	_, err := gitConfigOutput(ctx, file, key, value)
	return err
}

func gitConfigAdd(ctx context.Context, file, key, value string) error {
	_, err := gitConfigOutput(ctx, file, "--add", key, value)
	return err
}

func gitConfigOutput(ctx context.Context, file string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+3)
	cmdArgs = append(cmdArgs, "config", "--file", file)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", scrubCloneText(msg))
	}
	return stdout.String(), nil
}
