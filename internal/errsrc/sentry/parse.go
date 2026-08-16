package sentry

import (
	"net/url"
	"strings"

	"github.com/acoshift/grokwork/internal/errsrc"
)

// ParseURL recognizes verified sentry.io / self-hosted issue URLs. No free text.
func ParseURL(raw string) (errsrc.Ref, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<")
	raw = strings.TrimSuffix(raw, ">")
	if raw == "" {
		return errsrc.Ref{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return errsrc.Ref{}, false
	}
	path := strings.TrimSuffix(u.Path, "/")
	if i := strings.Index(path, "/events/"); i >= 0 {
		path = path[:i]
	}
	host := strings.ToLower(u.Host)
	// https://sentry.io/organizations/{org}/issues/{id}
	if host == "sentry.io" {
		if org, id, ok := cutOrgIssues(path); ok {
			return errsrc.Ref{Provider: errsrc.ProviderSentry, ID: id, ProjectHint: org}, true
		}
		// https://sentry.io/{org}/{project}/issues/{id}
		if org, id, ok := cutLegacyIssues(path); ok {
			return errsrc.Ref{Provider: errsrc.ProviderSentry, ID: id, ProjectHint: org}, true
		}
		// https://sentry.io/issues/{id}
		if id, ok := lastIssuesID(path); ok && strings.HasPrefix(path, "/issues/") {
			return errsrc.Ref{Provider: errsrc.ProviderSentry, ID: id}, true
		}
	}
	// https://{org}.sentry.io/issues/{id}
	if org, ok := strings.CutSuffix(host, ".sentry.io"); ok && org != "" && !strings.Contains(org, ".") {
		if id, ok := lastIssuesID(path); ok {
			return errsrc.Ref{Provider: errsrc.ProviderSentry, ID: id, ProjectHint: org}, true
		}
	}
	return errsrc.Ref{}, false
}

func cutOrgIssues(path string) (org, id string, ok bool) {
	// /organizations/{org}/issues/{id}
	const pfx = "/organizations/"
	if !strings.HasPrefix(path, pfx) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, pfx)
	org, rest, ok = strings.Cut(rest, "/issues/")
	if !ok || org == "" || rest == "" || strings.Contains(rest, "/") {
		return "", "", false
	}
	return org, rest, true
}

func cutLegacyIssues(path string) (org, id string, ok bool) {
	// /{org}/{project}/issues/{id}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" || parts[0] == "organizations" || parts[0] == "issues" {
		return "", "", false
	}
	if parts[0] == "" || parts[3] == "" {
		return "", "", false
	}
	return parts[0], parts[3], true
}

func lastIssuesID(path string) (string, bool) {
	const pfx = "/issues/"
	i := strings.LastIndex(path, pfx)
	if i < 0 {
		return "", false
	}
	id := path[i+len(pfx):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}
