package agentapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
)

// GCPErrorRow is an MCP list row.
type GCPErrorRow struct {
	ID       string `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	Count    int64  `json:"count,omitempty"`
	LastSeen string `json:"lastSeen,omitempty"`
	Service  string `json:"service,omitempty"`
	URL      string `json:"url,omitempty"`
}

// GCPError is the MCP get snapshot.
type GCPError struct {
	GCPErrorRow
	Sample string         `json:"sample,omitempty"`
	Recent []errsrc.Event `json:"recent,omitempty"`
}

func (s *Service) gcpClient(project string) (*gcperr.Client, error) {
	if s.GCPErrorsEnabled != nil && !s.GCPErrorsEnabled(project) {
		return nil, fmt.Errorf("gcp error reporting is not enabled for this project")
	}
	projectID := ""
	if s.GCPProjectID != nil {
		projectID = strings.TrimSpace(s.GCPProjectID(project))
	}
	if projectID == "" {
		return nil, fmt.Errorf("gcp: no projectId")
	}
	if s.GCPNew != nil {
		return s.GCPNew(project), nil
	}
	svc := ""
	if s.GCPService != nil {
		svc = s.GCPService(project)
	}
	creds := ""
	if s.GCPCredentialsFile != nil {
		creds = s.GCPCredentialsFile(project)
	}
	return &gcperr.Client{
		ProjectID: projectID,
		Service:   svc,
		Tokens:    gcperr.TokenSourceFor(creds),
	}, nil
}

// ListGCPErrors lists Error Reporting groups.
func (s *Service) ListGCPErrors(ctx context.Context, token, period, sort string, limit int, cursor, service string) ([]GCPErrorRow, string, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, "", err
	}
	if !cred.Caps.GCPErrorsRead {
		return nil, "", fmt.Errorf("forbidden: gcp errors read")
	}
	client, err := s.gcpClient(cred.Project)
	if err != nil {
		return nil, "", err
	}
	res, err := client.List(ctx, errsrc.ListQuery{TimeRange: period, Sort: sort, Limit: limit, Cursor: cursor, Service: service})
	if err != nil {
		return nil, "", err
	}
	out := make([]GCPErrorRow, 0, len(res.Groups))
	for _, g := range res.Groups {
		out = append(out, gcpRow(g))
	}
	return out, res.NextCursor, nil
}

// GetGCPError fetches one group via groupStats+events. Ref is group id or console URL.
func (s *Service) GetGCPError(ctx context.Context, token, ref, period string) (GCPError, error) {
	cred, err := s.verify(token)
	if err != nil {
		return GCPError{}, err
	}
	if !cred.Caps.GCPErrorsRead {
		return GCPError{}, fmt.Errorf("forbidden: gcp errors read")
	}
	id, hint, err := parseGCPRef(ref)
	if err != nil {
		return GCPError{}, err
	}
	if hint != "" {
		wantID, wantNum := "", ""
		if s.GCPProjectID != nil {
			wantID = s.GCPProjectID(cred.Project)
		}
		if s.GCPProjectNumber != nil {
			wantNum = s.GCPProjectNumber(cred.Project)
		}
		if !strings.EqualFold(hint, wantID) && (wantNum == "" || hint != wantNum) {
			return GCPError{}, fmt.Errorf("not found")
		}
	}
	client, err := s.gcpClient(cred.Project)
	if err != nil {
		return GCPError{}, err
	}
	got, err := client.Get(ctx, id, period)
	if err != nil {
		return GCPError{}, err
	}
	return GCPError{GCPErrorRow: gcpRow(got.Group), Sample: got.Sample, Recent: got.Recent}, nil
}

func parseGCPRef(ref string) (id, projectHint string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("ref required")
	}
	if parsed, ok := gcperr.ParseURL(ref); ok {
		return parsed.ID, parsed.ProjectHint, nil
	}
	if strings.Contains(ref, "/") || strings.Contains(ref, "://") {
		return "", "", fmt.Errorf("not a GCP group id")
	}
	return ref, "", nil
}

func gcpRow(g errsrc.Group) GCPErrorRow {
	return GCPErrorRow{
		ID: g.ID, Title: g.Title, Count: g.Count,
		LastSeen: formatTime(g.LastSeen), Service: g.Resource, URL: g.URL,
	}
}
