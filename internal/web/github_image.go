package web

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/markdown"
)

const maxGitHubImageBytes = 20 << 20

// githubImage is GET /projects/{project}/github-images?u=.
// Private GitHub attachments 404 in the browser (SameSite cookies never ride
// a cross-origin <img>). The host gh token fetches them; the page only
// rewrites allowlisted attachment URLs here.
func (s *Server) githubImage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	raw := strings.TrimSpace(ctx.FormValue("u"))
	if !githubImageURLAllowed(raw) {
		return ctx.Status(http.StatusBadRequest).Error("unsupported image url")
	}
	ctype, body, err := s.fetchGitHubImage(ctx.Context(), raw)
	if err != nil {
		return ctx.Status(http.StatusBadGateway).Error("image fetch failed")
	}
	if !allowedImageType(ctype) {
		ctype = sniffImageType(body)
	}
	if !allowedImageType(ctype) {
		return ctx.Status(http.StatusUnsupportedMediaType).Error("not an image")
	}
	w := ctx.ResponseWriter()
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

func (s *Server) githubMarkdown(project, src string) template.HTML {
	project = strings.TrimSpace(project)
	return markdown.RenderRewriting(src, func(img string) string {
		if project == "" || !githubImageURLAllowed(img) {
			return img
		}
		return "/projects/" + url.PathEscape(project) + "/github-images?u=" + url.QueryEscape(img)
	})
}

func (s *Server) fetchGitHubImage(ctx context.Context, rawURL string) (string, []byte, error) {
	if s != nil && s.githubImageGet != nil {
		return s.githubImageGet(ctx, rawURL)
	}
	token, err := s.githubAuthToken(ctx)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "grokwork")
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	if githubImageNeedsAuth(req.URL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := githubImageHTTPClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("github image: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubImageBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(body) > maxGitHubImageBytes {
		return "", nil, fmt.Errorf("github image: too large")
	}
	ctype := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	return strings.TrimSpace(strings.ToLower(ctype)), body, nil
}

func (s *Server) githubAuthToken(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("github auth: no server")
	}
	s.ghTokenMu.Lock()
	defer s.ghTokenMu.Unlock()
	if s.ghToken != "" && time.Now().Before(s.ghTokenUntil) {
		return s.ghToken, nil
	}
	var raw []byte
	var err error
	if run := s.ghRun(); run != nil {
		raw, err = run(ctx, "", "gh", "auth", "token")
	} else {
		raw, err = exec.CommandContext(ctx, "gh", "auth", "token").Output()
	}
	tok := strings.TrimSpace(string(raw))
	if err != nil {
		return "", fmt.Errorf("gh auth token: failed")
	}
	if tok == "" {
		return "", fmt.Errorf("gh auth token: empty")
	}
	s.ghToken = tok
	s.ghTokenUntil = time.Now().Add(2 * time.Minute)
	return tok, nil
}

func githubImageNeedsAuth(u *url.URL) bool {
	if u == nil {
		return false
	}
	switch strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") {
	case "github.com", "www.github.com":
		return true
	default:
		return false
	}
}

func githubImageURLAllowed(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.User != nil || u.Opaque != "" {
		return false
	}
	if strings.ToLower(u.Scheme) != "https" {
		return false
	}
	if u.Port() != "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	path := u.EscapedPath()
	if path == "" {
		path = u.Path
	}
	if strings.Contains(path, "..") || strings.Contains(path, "//") || strings.Contains(path, "@") {
		return false
	}
	switch host {
	case "github.com", "www.github.com":
		return githubAttachmentPath(path)
	case "user-images.githubusercontent.com",
		"private-user-images.githubusercontent.com",
		"objects.githubusercontent.com",
		"objects-origin.githubusercontent.com":
		return len(path) > 1 && strings.HasPrefix(path, "/")
	default:
		return false
	}
}

func githubAttachmentPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	const assets = "/user-attachments/assets/"
	if rest, ok := strings.CutPrefix(path, assets); ok {
		return rest != "" && !strings.Contains(rest, "/") && githubAssetID(rest)
	}
	const files = "/user-attachments/files/"
	if rest, ok := strings.CutPrefix(path, files); ok {
		id, name, ok := strings.Cut(rest, "/")
		if !ok || !githubAssetID(id) || name == "" || strings.Contains(name, "/") {
			return false
		}
		return imageFileExt(name)
	}
	return false
}

func githubAssetID(s string) bool {
	if s == "" || len(s) > 80 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func imageFileExt(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	switch strings.ToLower(name[i+1:]) {
	case "png", "jpg", "jpeg", "gif", "webp":
		return true
	default:
		return false
	}
}

func allowedImageType(ctype string) bool {
	switch strings.ToLower(strings.TrimSpace(ctype)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func sniffImageType(body []byte) string {
	switch {
	case len(body) >= 8 && string(body[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(body) >= 3 && string(body[:3]) == "\xff\xd8\xff":
		return "image/jpeg"
	case len(body) >= 6 && (string(body[:6]) == "GIF87a" || string(body[:6]) == "GIF89a"):
		return "image/gif"
	case len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	return true
}

var githubImageHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if !publicIP(ip) {
					return fmt.Errorf("blocked address")
				}
				return nil
			},
		}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL == nil || !githubImageURLAllowed(req.URL.String()) {
			return fmt.Errorf("redirect off allowlist")
		}
		// Do not forward the PAT off github.com. githubusercontent hops
		// carry a jwt on the URL; Authorization is only for the first hop.
		req.Header.Del("Authorization")
		if githubImageNeedsAuth(req.URL) && len(via) > 0 {
			if tok := via[0].Header.Get("Authorization"); tok != "" {
				req.Header.Set("Authorization", tok)
			}
		}
		return nil
	},
}
