package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/markdown"
)

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
	if !ghpr.UserAssetURLAllowed(raw) {
		return ctx.Status(http.StatusBadRequest).Error("unsupported image url")
	}
	ctype, body, err := s.fetchGitHubImage(ctx.Context(), raw)
	if err != nil {
		if errors.Is(err, ghpr.ErrNotImage) {
			return ctx.Status(http.StatusUnsupportedMediaType).Error("not an image")
		}
		fmt.Fprintf(os.Stderr, "web: github image fetch failed: %v\n", err)
		return ctx.Status(http.StatusBadGateway).Error("image fetch failed")
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
		if project == "" || !ghpr.UserAssetURLAllowed(img) {
			return img
		}
		return "/projects/" + url.PathEscape(project) + "/github-images?u=" + url.QueryEscape(img)
	})
}

type githubImageCacheEntry struct {
	ctype string
	body  []byte
	until time.Time
}

const githubImageCacheTTL = time.Hour
const githubImageCacheMax = 16

func (s *Server) fetchGitHubImage(ctx context.Context, rawURL string) (string, []byte, error) {
	if ctype, body, ok := s.lookupGitHubImage(rawURL); ok {
		return ctype, body, nil
	}
	var ctype string
	var body []byte
	var err error
	if s != nil && s.githubImageGet != nil {
		ctype, body, err = s.githubImageGet(ctx, rawURL)
	} else {
		var token string
		token, err = s.githubAuthToken(ctx)
		if err != nil {
			return "", nil, err
		}
		ctype, body, err = ghpr.FetchUserAsset(ctx, token, rawURL)
	}
	if err != nil {
		return "", nil, err
	}
	if !ghpr.AllowedImageType(ctype) {
		ctype = ghpr.SniffImageType(body)
	}
	if !ghpr.AllowedImageType(ctype) {
		return "", nil, ghpr.ErrNotImage
	}
	s.storeGitHubImage(rawURL, ctype, body)
	return ctype, body, nil
}

func (s *Server) lookupGitHubImage(rawURL string) (string, []byte, bool) {
	if s == nil {
		return "", nil, false
	}
	s.githubImgMu.Lock()
	defer s.githubImgMu.Unlock()
	ent, ok := s.githubImgCache[rawURL]
	if !ok || time.Now().After(ent.until) {
		return "", nil, false
	}
	return ent.ctype, ent.body, true
}

func (s *Server) storeGitHubImage(rawURL, ctype string, body []byte) {
	if s == nil || rawURL == "" || len(body) == 0 {
		return
	}
	s.githubImgMu.Lock()
	defer s.githubImgMu.Unlock()
	if s.githubImgCache == nil {
		s.githubImgCache = make(map[string]githubImageCacheEntry)
	}
	if len(s.githubImgCache) >= githubImageCacheMax {
		clear(s.githubImgCache)
	}
	s.githubImgCache[rawURL] = githubImageCacheEntry{
		ctype: ctype,
		body:  body,
		until: time.Now().Add(githubImageCacheTTL),
	}
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
	tok, err := ghpr.AuthToken(ctx, s.ghRun())
	if err != nil {
		return "", err
	}
	s.ghToken = tok
	s.ghTokenUntil = time.Now().Add(2 * time.Minute)
	return tok, nil
}

func githubImageURLAllowed(raw string) bool {
	return ghpr.UserAssetURLAllowed(raw)
}

func githubIssueImageText(info ghpr.IssueInfo) string {
	var b strings.Builder
	b.WriteString(info.Body)
	for _, c := range info.Comments {
		b.WriteByte('\n')
		b.WriteString(c.Body)
	}
	return b.String()
}
