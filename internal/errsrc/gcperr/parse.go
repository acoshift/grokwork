package gcperr

import (
	"net/url"
	"strings"

	"github.com/acoshift/grokwork/internal/errsrc"
)

// ParseURL recognizes Cloud Console error detail URLs.
func ParseURL(raw string) (errsrc.Ref, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	raw = strings.TrimSuffix(raw, ">")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return errsrc.Ref{}, false
	}
	if !strings.EqualFold(u.Host, "console.cloud.google.com") {
		return errsrc.Ref{}, false
	}
	path := u.Path
	const pfx = "/errors/detail/"
	if !strings.HasPrefix(path, pfx) {
		return errsrc.Ref{}, false
	}
	seg := strings.TrimPrefix(path, pfx)
	if i := strings.Index(seg, "/"); i >= 0 {
		return errsrc.Ref{}, false
	}
	if i := strings.Index(seg, ";"); i >= 0 {
		seg = seg[:i]
	}
	if seg == "" {
		return errsrc.Ref{}, false
	}
	project := u.Query().Get("project")
	return errsrc.Ref{Provider: errsrc.ProviderGCP, ID: seg, ProjectHint: project}, true
}
