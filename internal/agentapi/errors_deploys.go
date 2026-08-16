package agentapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
)

// DeploysErrorRow is an MCP list row (no sample stack).
type DeploysErrorRow struct {
	ID          string `json:"id,omitempty"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Count       int64  `json:"count,omitempty"`
	FirstSeen   string `json:"firstSeen,omitempty"`
	LastSeen    string `json:"lastSeen,omitempty"`
	URL         string `json:"url,omitempty"`
	Location    string `json:"location,omitempty"`
	Deployment  string `json:"deployment,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// DeploysErrorEvent is one occurrence pointer (no secrets).
type DeploysErrorEvent struct {
	Timestamp string `json:"timestamp,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Extra     string `json:"extra,omitempty"`
}

// DeploysError is the MCP get snapshot (sample capped).
type DeploysError struct {
	DeploysErrorRow
	Sample string              `json:"sample,omitempty"`
	Recent []DeploysErrorEvent `json:"recent,omitempty"`
}

// DeploysErrorList is one page of list rows.
type DeploysErrorList struct {
	Issues     []DeploysErrorRow `json:"issues,omitempty"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func (s *Service) deploysClient(project string) (*deploys.Client, string, error) {
	if s.DeploysErrorsEnabled != nil && !s.DeploysErrorsEnabled(project) {
		return nil, "", fmt.Errorf("deploys.app errors are not enabled for this project")
	}
	depProject := ""
	if s.DeploysProject != nil {
		depProject = strings.TrimSpace(s.DeploysProject(project))
	}
	if depProject == "" {
		return nil, "", fmt.Errorf("deploys: no project configured")
	}
	opts := deploys.Options{}
	if s.DeploysAPIToken != nil {
		opts.Token = s.DeploysAPIToken(project)
	}
	if s.DeploysBasicUser != nil {
		opts.BasicUser = s.DeploysBasicUser(project)
	}
	if s.DeploysBasicPass != nil {
		opts.BasicPass = s.DeploysBasicPass(project)
	}
	if strings.TrimSpace(opts.Token) == "" && (opts.BasicUser == "" || opts.BasicPass == "") {
		return nil, "", fmt.Errorf("deploys: no API token")
	}
	if s.DeploysNew != nil {
		return s.DeploysNew(opts), depProject, nil
	}
	return deploys.New(opts), depProject, nil
}

func (s *Service) deploysDefaults(project string) (location, name string) {
	if s.DeploysLocation != nil {
		location = strings.TrimSpace(s.DeploysLocation(project))
	}
	if s.DeploysDeployment != nil {
		name = strings.TrimSpace(s.DeploysDeployment(project))
	}
	return location, name
}

// ListDeploysErrors lists error groups for the configured deploys.app project.
// Optional location/name narrow that project; they cannot name another one.
func (s *Service) ListDeploysErrors(ctx context.Context, token, status string, limit int, cursor, sort, location, name string) (DeploysErrorList, error) {
	cred, err := s.verify(token)
	if err != nil {
		return DeploysErrorList{}, err
	}
	if !cred.Caps.DeploysErrorsRead {
		return DeploysErrorList{}, fmt.Errorf("forbidden: deploys errors read")
	}
	client, depProject, err := s.deploysClient(cred.Project)
	if err != nil {
		return DeploysErrorList{}, err
	}
	loc, dep := s.deploysDefaults(cred.Project)
	if l := strings.TrimSpace(location); l != "" {
		loc = l
	}
	if n := strings.TrimSpace(name); n != "" {
		dep = n
	}
	res, err := client.List(ctx, deploys.ListReq{
		Project:  depProject,
		Location: loc,
		Name:     dep,
		Status:   strings.TrimSpace(status),
		Limit:    limit,
		Cursor:   strings.TrimSpace(cursor),
		Sort:     strings.TrimSpace(sort),
	})
	if err != nil {
		return DeploysErrorList{}, err
	}
	out := DeploysErrorList{NextCursor: res.NextCursor}
	out.Issues = make([]DeploysErrorRow, 0, len(res.Groups))
	for _, g := range res.Groups {
		out.Issues = append(out.Issues, rowFromGroup(g))
	}
	return out, nil
}

// GetDeploysError resolves ref as location/name/id, a verified console URL, or a
// bare id (bare id may use configured default location+deployment).
func (s *Service) GetDeploysError(ctx context.Context, token, ref string) (DeploysError, error) {
	cred, err := s.verify(token)
	if err != nil {
		return DeploysError{}, err
	}
	if !cred.Caps.DeploysErrorsRead {
		return DeploysError{}, fmt.Errorf("forbidden: deploys errors read")
	}
	client, depProject, err := s.deploysClient(cred.Project)
	if err != nil {
		return DeploysError{}, err
	}
	defLoc, defName := s.deploysDefaults(cred.Project)
	loc, name, id, hint, err := parseDeploysRef(ref, defLoc, defName)
	if err != nil {
		return DeploysError{}, err
	}
	if hint != "" && hint != depProject {
		return DeploysError{}, fmt.Errorf("not found")
	}
	got, err := client.Get(ctx, deploys.GetReq{
		Project:  depProject,
		Location: loc,
		Name:     name,
		ID:       id,
	})
	if err != nil {
		return DeploysError{}, err
	}
	return detailFromGroup(got), nil
}

func parseDeploysRef(ref, defaultLoc, defaultName string) (location, name, id, projectHint string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", "", fmt.Errorf("ref required")
	}
	if parsed, ok := deploys.ParseURL(ref); ok {
		return parsed.Location, parsed.Resource, parsed.ID, parsed.ProjectHint, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) == 3 {
		loc, nam, ident := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if loc != "" && nam != "" && ident != "" {
			return loc, nam, ident, "", nil
		}
	}
	if strings.Contains(ref, "/") {
		return "", "", "", "", fmt.Errorf("ref must be id, location/name/id, or a console URL")
	}
	if strings.TrimSpace(defaultLoc) == "" || strings.TrimSpace(defaultName) == "" {
		return "", "", "", "", fmt.Errorf("bare id requires configured default location and deployment")
	}
	return defaultLoc, defaultName, ref, "", nil
}

func rowFromGroup(g errsrc.Group) DeploysErrorRow {
	return DeploysErrorRow{
		ID:          g.ID,
		Title:       g.Title,
		Status:      g.Status,
		Kind:        g.Culprit,
		Count:       g.Count,
		FirstSeen:   formatTime(g.FirstSeen),
		LastSeen:    formatTime(g.LastSeen),
		URL:         g.URL,
		Location:    g.Location,
		Deployment:  g.Resource,
		Fingerprint: g.Fingerprint,
	}
}

func detailFromGroup(d errsrc.GroupDetail) DeploysError {
	out := DeploysError{DeploysErrorRow: rowFromGroup(d.Group), Sample: errsrc.CapSample(d.Sample)}
	if len(d.Recent) > 0 {
		out.Recent = make([]DeploysErrorEvent, 0, len(d.Recent))
		for _, ev := range d.Recent {
			out.Recent = append(out.Recent, DeploysErrorEvent{
				Timestamp: formatTime(ev.Timestamp),
				Pod:       ev.Culprit,
				Extra:     ev.Extra,
			})
		}
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
