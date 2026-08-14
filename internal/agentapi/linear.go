package agentapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const linearDescMaxRunes = 8000

// LinearIssue is the MCP-facing snapshot (no secrets).
type LinearIssue struct {
	ID          string `json:"id,omitempty"`
	Identifier  string `json:"identifier,omitempty"`
	Title       string `json:"title,omitempty"`
	State       string `json:"state,omitempty"`
	URL         string `json:"url,omitempty"`
	TeamKey     string `json:"teamKey,omitempty"`
	Description string `json:"description,omitempty"`
}

// LinearIssueRow is a list row without description.
type LinearIssueRow struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	URL        string `json:"url,omitempty"`
	TeamKey    string `json:"teamKey,omitempty"`
}

func issueFromLinear(iss linear.Issue) LinearIssue {
	return LinearIssue{
		ID:          iss.ID,
		Identifier:  iss.Identifier,
		Title:       iss.Title,
		State:       iss.State,
		URL:         iss.URL,
		TeamKey:     iss.TeamKey,
		Description: capRunes(iss.Description, linearDescMaxRunes),
	}
}

func (s *Service) linearClient(project string) (*linear.Client, error) {
	if s.LinearEnabled != nil && !s.LinearEnabled(project) {
		return nil, fmt.Errorf("linear is not enabled for this project")
	}
	key := ""
	if s.LinearAPIKey != nil {
		key = s.LinearAPIKey(project)
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("linear: no API key")
	}
	if s.LinearNew != nil {
		return s.LinearNew(key), nil
	}
	return linear.New(key), nil
}

// GetLinearIssue resolves one Linear identifier or issue URL for the token's project.
func (s *Service) GetLinearIssue(ctx context.Context, token, ref string) (LinearIssue, error) {
	cred, err := s.verify(token)
	if err != nil {
		return LinearIssue{}, err
	}
	if !cred.Caps.LinearRead {
		return LinearIssue{}, fmt.Errorf("forbidden: linear read")
	}
	ident, err := parseLinearRef(ref)
	if err != nil {
		return LinearIssue{}, err
	}
	client, err := s.linearClient(cred.Project)
	if err != nil {
		return LinearIssue{}, err
	}
	got, err := client.GetByIdentifier(ctx, ident)
	if err != nil {
		return LinearIssue{}, err
	}
	return issueFromLinear(got), nil
}

// ListLinearIssues lists recent issues for the bound project's Linear team key.
func (s *Service) ListLinearIssues(ctx context.Context, token string, limit int) ([]LinearIssueRow, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.LinearRead {
		return nil, fmt.Errorf("forbidden: linear read")
	}
	team := ""
	if s.LinearTeamKey != nil {
		team = s.LinearTeamKey(cred.Project)
	}
	if strings.TrimSpace(team) == "" {
		return nil, fmt.Errorf("linear: no team key")
	}
	client, err := s.linearClient(cred.Project)
	if err != nil {
		return nil, err
	}
	issues, err := client.ListTeamIssues(ctx, team, limit)
	if err != nil {
		return nil, err
	}
	out := make([]LinearIssueRow, 0, len(issues))
	for _, iss := range issues {
		out = append(out, LinearIssueRow{
			ID: iss.ID, Identifier: iss.Identifier, Title: iss.Title,
			State: iss.State, URL: iss.URL, TeamKey: iss.TeamKey,
		})
	}
	return out, nil
}

// parseLinearRef requires exactly one Linear identifier (TEAM-N or a Linear issue URL).
func parseLinearRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("ref required")
	}
	parsed := sessionstore.ParseLinearIssueRefs(ref)
	if len(parsed) == 0 {
		return "", fmt.Errorf("not a Linear identifier")
	}
	if len(parsed) != 1 {
		return "", fmt.Errorf("multiple Linear refs")
	}
	id := sessionstore.NormalizeLinearIdentifier(parsed[0].Identifier)
	if id == "" {
		return "", fmt.Errorf("not a Linear identifier")
	}
	return id, nil
}
