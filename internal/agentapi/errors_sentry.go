package agentapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/sentry"
)

// SentryIssueRow is an MCP list row.
type SentryIssueRow struct {
	ID        string `json:"id,omitempty"`
	ShortID   string `json:"shortId,omitempty"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	Culprit   string `json:"culprit,omitempty"`
	Count     int64  `json:"count,omitempty"`
	UserCount int64  `json:"userCount,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
	URL       string `json:"url,omitempty"`
}

// SentryIssue is the MCP get snapshot.
type SentryIssue struct {
	SentryIssueRow
	Sample string `json:"sample,omitempty"`
}

func (s *Service) sentryClient(project string) (*sentry.Client, error) {
	if s.SentryEnabled != nil && !s.SentryEnabled(project) {
		return nil, fmt.Errorf("sentry is not enabled for this project")
	}
	token := ""
	if s.SentryAuthToken != nil {
		token = s.SentryAuthToken(project)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("sentry: no API token")
	}
	org, proj, base := "", "", ""
	if s.SentryOrg != nil {
		org = s.SentryOrg(project)
	}
	if s.SentryProject != nil {
		proj = s.SentryProject(project)
	}
	if s.SentryBaseURL != nil {
		base = s.SentryBaseURL(project)
	}
	if org == "" || proj == "" {
		return nil, fmt.Errorf("sentry: org and project required")
	}
	if s.SentryNew != nil {
		return s.SentryNew(token, org, proj, base), nil
	}
	return sentry.New(token, org, proj, base), nil
}

// ListSentryIssues lists issues for the configured org/project only.
func (s *Service) ListSentryIssues(ctx context.Context, token, query string, limit int, cursor, sort string) ([]SentryIssueRow, string, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, "", err
	}
	if !cred.Caps.SentryRead {
		return nil, "", fmt.Errorf("forbidden: sentry read")
	}
	client, err := s.sentryClient(cred.Project)
	if err != nil {
		return nil, "", err
	}
	res, err := client.List(ctx, errsrc.ListQuery{Status: query, Limit: limit, Cursor: cursor, Sort: sort})
	if err != nil {
		return nil, "", err
	}
	out := make([]SentryIssueRow, 0, len(res.Groups))
	for _, g := range res.Groups {
		out = append(out, sentryRow(g))
	}
	return out, res.NextCursor, nil
}

// GetSentryIssue fetches via the org-scoped route and post-checks project slug.
func (s *Service) GetSentryIssue(ctx context.Context, token, ref string) (SentryIssue, error) {
	cred, err := s.verify(token)
	if err != nil {
		return SentryIssue{}, err
	}
	if !cred.Caps.SentryRead {
		return SentryIssue{}, fmt.Errorf("forbidden: sentry read")
	}
	id, hint, err := parseSentryRef(ref)
	if err != nil {
		return SentryIssue{}, err
	}
	cfgOrg := ""
	if s.SentryOrg != nil {
		cfgOrg = s.SentryOrg(cred.Project)
	}
	if hint != "" && !strings.EqualFold(hint, cfgOrg) {
		return SentryIssue{}, fmt.Errorf("not found")
	}
	client, err := s.sentryClient(cred.Project)
	if err != nil {
		return SentryIssue{}, err
	}
	got, slug, err := client.Get(ctx, id)
	if err != nil {
		return SentryIssue{}, err
	}
	want := ""
	if s.SentryProject != nil {
		want = s.SentryProject(cred.Project)
	}
	if !sentry.MatchProject(slug, want) {
		return SentryIssue{}, fmt.Errorf("not found")
	}
	return SentryIssue{SentryIssueRow: sentryRow(got.Group), Sample: got.Sample}, nil
}

func parseSentryRef(ref string) (id, orgHint string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("ref required")
	}
	if parsed, ok := sentry.ParseURL(ref); ok {
		return parsed.ID, parsed.ProjectHint, nil
	}
	if strings.Contains(ref, "/") || strings.Contains(ref, "://") {
		return "", "", fmt.Errorf("not a Sentry identifier")
	}
	return ref, "", nil
}

func sentryRow(g errsrc.Group) SentryIssueRow {
	return SentryIssueRow{
		ID: g.ID, ShortID: g.ShortID, Title: g.Title, Status: g.Status,
		Culprit: g.Culprit, Count: g.Count, UserCount: g.UserCount,
		LastSeen: formatTime(g.LastSeen), URL: g.URL,
	}
}
