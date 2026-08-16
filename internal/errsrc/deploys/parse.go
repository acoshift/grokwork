package deploys

import (
	"net/url"
	"strings"

	"github.com/acoshift/grokwork/internal/errsrc"
)

const consoleHost = "console.deploys.app"
const consolePath = "/deployment/errors"

// ParseURL accepts only
// https://console.deploys.app/deployment/errors?project=&location=&name=&id=.
// Discord <> wraps are stripped. Extra query params are ignored.
func ParseURL(raw string) (errsrc.Ref, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	raw = strings.TrimSuffix(raw, ">")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errsrc.Ref{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errsrc.Ref{}, false
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errsrc.Ref{}, false
	}
	if !strings.EqualFold(u.Host, consoleHost) {
		return errsrc.Ref{}, false
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path != consolePath {
		return errsrc.Ref{}, false
	}
	q := u.Query()
	project := strings.TrimSpace(q.Get("project"))
	loc := strings.TrimSpace(q.Get("location"))
	name := strings.TrimSpace(q.Get("name"))
	id := strings.TrimSpace(q.Get("id"))
	if project == "" || loc == "" || name == "" || id == "" {
		return errsrc.Ref{}, false
	}
	return errsrc.Ref{
		Provider:    errsrc.ProviderDeploys,
		ID:          id,
		Location:    loc,
		Resource:    name,
		ProjectHint: project,
	}, true
}

// ConsoleURL builds the verified console permalink for one issue.
func ConsoleURL(project, location, name, id string) string {
	q := url.Values{}
	q.Set("project", project)
	q.Set("location", location)
	q.Set("name", name)
	q.Set("id", id)
	return "https://" + consoleHost + consolePath + "?" + q.Encode()
}
