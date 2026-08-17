package ghpr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// ErrNotImage is returned when the fetched bytes are not a raster image.
var ErrNotImage = errors.New("github image: not an image")

// MaxUserAssetBytes caps one GitHub issue/PR image fetch.
const MaxUserAssetBytes = 20 << 20

// MaxUserAssetsPerBody caps how many images we pull out of one issue body.
const MaxUserAssetsPerBody = 10

var userAssetURLRe = regexp.MustCompile(`https://[^\s)>"']+`)

// UserAssetURLAllowed reports whether raw is a GitHub user-attachment (or the
// S3/githubusercontent hop those URLs 302 to). Not a general GitHub URL
// allowlist — raw repo paths stay out.
func UserAssetURLAllowed(raw string) bool {
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
		return githubUserAssetS3Host(host) && len(path) > 1 && strings.HasPrefix(path, "/")
	}
}

// ExtractUserAssetURLs returns allowlisted GitHub image URLs in src, first
// seen order, capped at MaxUserAssetsPerBody.
func ExtractUserAssetURLs(src string) []string {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range userAssetURLRe.FindAllString(src, -1) {
		raw = strings.TrimRight(raw, ".,;]")
		if !UserAssetURLAllowed(raw) {
			continue
		}
		key := strings.TrimSuffix(raw, "/")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
		if len(out) >= MaxUserAssetsPerBody {
			break
		}
	}
	return out
}

// AuthToken returns the host gh token. run nil uses exec (same as the web proxy).
func AuthToken(ctx context.Context, run Runner) (string, error) {
	var raw []byte
	var err error
	if run != nil {
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
	return tok, nil
}

// GitHub's user-attachments CDN 503s generic clients (User-Agent grokwork,
// Accept: image/*). The same URL 302s to S3 when we look like `gh`.
const (
	userAssetUserAgent = "GitHub CLI"
	userAssetAccept    = "application/vnd.github.raw"
)

// FetchUserAsset GETs an allowlisted GitHub attachment using token on github.com
// hops only. Redirects must stay on the allowlist (S3 user-asset buckets).
func FetchUserAsset(ctx context.Context, token, rawURL string) (string, []byte, error) {
	if !UserAssetURLAllowed(rawURL) {
		return "", nil, fmt.Errorf("unsupported image url")
	}
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(attempt) * 250 * time.Millisecond
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		ctype, body, err := fetchUserAssetOnce(ctx, token, rawURL)
		if err == nil {
			return ctype, body, nil
		}
		last = err
		if !userAssetRetryable(err) {
			return "", nil, err
		}
	}
	return "", nil, last
}

func userAssetRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 502") || strings.Contains(s, "HTTP 503") || strings.Contains(s, "HTTP 504")
}

func fetchUserAssetOnce(ctx context.Context, token, rawURL string) (string, []byte, error) {
	req, err := newUserAssetRequest(ctx, token, rawURL)
	if err != nil {
		return "", nil, err
	}
	resp, err := userAssetHTTPClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("github image: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxUserAssetBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(body) > MaxUserAssetBytes {
		return "", nil, fmt.Errorf("github image: too large")
	}
	ctype := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	ctype = strings.TrimSpace(strings.ToLower(ctype))
	if !AllowedImageType(ctype) {
		if sniffed := SniffImageType(body); sniffed != "" {
			ctype = sniffed
		}
	}
	if !AllowedImageType(ctype) {
		return "", nil, ErrNotImage
	}
	return ctype, body, nil
}

func newUserAssetRequest(ctx context.Context, token, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAssetUserAgent)
	req.Header.Set("Accept", userAssetAccept)
	if u, err := url.Parse(rawURL); err == nil && userAssetNeedsAuth(u) && strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	return req, nil
}

// AllowedImageType is the raster set we will serve or hand to the agent.
func AllowedImageType(ctype string) bool {
	switch strings.ToLower(strings.TrimSpace(ctype)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// SniffImageType returns a raster MIME from magic bytes, or empty.
func SniffImageType(body []byte) string {
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

// ImageExtForType maps a sniffed/allowlisted type to a filename suffix.
func ImageExtForType(ctype string) string {
	switch strings.ToLower(strings.TrimSpace(ctype)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func userAssetNeedsAuth(u *url.URL) bool {
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

func githubUserAssetS3Host(host string) bool {
	const prefix = "github-production-user-asset-"
	if !strings.HasPrefix(host, prefix) {
		return false
	}
	rest := host[len(prefix):]
	id, rest, ok := strings.Cut(rest, ".")
	if !ok || id == "" {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'f') && (c < '0' || c > '9') {
			return false
		}
	}
	if rest == "s3.amazonaws.com" {
		return true
	}
	if !strings.HasPrefix(rest, "s3.") || !strings.HasSuffix(rest, ".amazonaws.com") {
		return false
	}
	region := strings.TrimSuffix(strings.TrimPrefix(rest, "s3."), ".amazonaws.com")
	if region == "" || strings.ContainsAny(region, "/:@") {
		return false
	}
	for _, c := range region {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
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

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	return true
}

var userAssetHTTPClient = &http.Client{
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
		if req.URL == nil || !UserAssetURLAllowed(req.URL.String()) {
			host := ""
			if req.URL != nil {
				host = req.URL.Hostname()
			}
			return fmt.Errorf("redirect off allowlist (%s)", host)
		}
		req.Header.Del("Authorization")
		if userAssetNeedsAuth(req.URL) {
			if len(via) > 0 {
				if tok := via[0].Header.Get("Authorization"); tok != "" {
					req.Header.Set("Authorization", tok)
				}
			}
			req.Header.Set("Accept", userAssetAccept)
		} else {
			// S3 signed URLs: do not send the GitHub raw Accept.
			req.Header.Set("Accept", "*/*")
		}
		return nil
	},
}
