package bot

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/ghpr"
)

func (b *Bot) withFixImages(opts FixStartOpts, fn func([]string) (FixStartResult, error)) (FixStartResult, error) {
	paths, cleanup := b.stageGitHubIssueImages(opts)
	handed := false
	defer func() {
		if !handed {
			cleanup()
		}
	}()
	res, err := fn(paths)
	if err == nil {
		handed = true
	}
	return res, err
}

func (b *Bot) stageGitHubIssueImages(opts FixStartOpts) (paths []string, cleanup func()) {
	noop := func() {}
	if opts.Kind != FixKindGitHub {
		return nil, noop
	}
	urls := ghpr.ExtractUserAssetURLs(fixImageScanText(opts))
	if len(urls) == 0 {
		return nil, noop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	uploads := make([]WebUpload, 0, len(urls))
	for i, raw := range urls {
		ctype, body, err := b.fetchGitHubIssueImage(ctx, raw)
		if err != nil {
			log.Printf("fix: skip issue image: %v", err)
			continue
		}
		img := body
		name := githubIssueImageName(raw, ctype, i)
		uploads = append(uploads, WebUpload{
			Filename: name,
			Size:     int64(len(img)),
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(img)), nil
			},
		})
	}
	if len(uploads) == 0 {
		return nil, noop
	}
	paths, cleanup, err := b.SaveWebAttachments(uploads)
	if err != nil {
		log.Printf("fix: stage issue images: %v", err)
		return nil, noop
	}
	return paths, cleanup
}

func fixImageScanText(opts FixStartOpts) string {
	if s := strings.TrimSpace(opts.ImageText); s != "" {
		return s
	}
	return opts.Body
}

func (b *Bot) fetchGitHubIssueImage(ctx context.Context, rawURL string) (string, []byte, error) {
	if b != nil && b.githubAssetGet != nil {
		return b.githubAssetGet(ctx, rawURL)
	}
	tok, err := ghpr.AuthToken(ctx, b.ghRun())
	if err != nil {
		return "", nil, err
	}
	return ghpr.FetchUserAsset(ctx, tok, rawURL)
}

func githubIssueImageName(rawURL, ctype string, i int) string {
	base := "issue-image"
	if u, err := url.Parse(rawURL); err == nil {
		if leaf := path.Base(u.Path); leaf != "" && leaf != "/" && leaf != "." {
			base = leaf
		}
	}
	ext := ghpr.ImageExtForType(ctype)
	if filepath.Ext(base) == "" && ext != "" {
		base += ext
	}
	if i == 0 {
		return base
	}
	return uniqueFilename(base, map[string]int{base: i})
}
