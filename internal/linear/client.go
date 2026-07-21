// Package linear is a thin GraphQL client for Linear issue resolve (L1).
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultEndpoint = "https://api.linear.app/graphql"

// Client talks to Linear GraphQL with a personal API key.
type Client struct {
	APIKey   string
	Endpoint string
	HTTP     *http.Client
}

// New returns a client for the given API key (empty key → nil operations fail).
func New(apiKey string) *Client {
	return &Client{
		APIKey:   strings.TrimSpace(apiKey),
		Endpoint: defaultEndpoint,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
	}
}

// Issue is a resolved Linear issue snapshot.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	URL         string
	State       string
	TeamKey     string
	Description string
	// WorkState is set by the web layer (not Linear): "FIXING" when a
	// non-terminal Grok session binds this issue with Fixes.
	WorkState string
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, dest any) error {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("linear: missing API key")
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("linear: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return fmt.Errorf("linear: decode: %w", err)
	}
	if len(gr.Errors) > 0 {
		return fmt.Errorf("linear: %s", gr.Errors[0].Message)
	}
	if dest == nil {
		return nil
	}
	if len(gr.Data) == 0 || string(gr.Data) == "null" {
		return fmt.Errorf("linear: empty data")
	}
	return json.Unmarshal(gr.Data, dest)
}

// GetByIdentifier resolves TEAM-123 via filter on team key + number.
func (c *Client) GetByIdentifier(ctx context.Context, identifier string) (Issue, error) {
	identifier = strings.TrimSpace(identifier)
	team, num, ok := splitIdentifier(identifier)
	if !ok {
		return Issue{}, fmt.Errorf("linear: invalid identifier %q", identifier)
	}
	const q = `
query IssueByIdentifier($team: String!, $number: Float!) {
  issues(
    filter: {
      team: { key: { eq: $team } }
      number: { eq: $number }
    }
    first: 1
  ) {
    nodes {
      id
      identifier
      title
      url
      description
      state { name }
      team { key }
    }
  }
}`
	var data struct {
		Issues struct {
			Nodes []struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
				Team struct {
					Key string `json:"key"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.do(ctx, q, map[string]any{"team": team, "number": float64(num)}, &data); err != nil {
		return Issue{}, err
	}
	if len(data.Issues.Nodes) == 0 {
		return Issue{}, fmt.Errorf("linear: issue %s not found", identifier)
	}
	n := data.Issues.Nodes[0]
	return Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		URL:         n.URL,
		State:       n.State.Name,
		TeamKey:     n.Team.Key,
		Description: n.Description,
	}, nil
}

// ListTeamIssues returns recent issues for a team key (e.g. ENG).
func (c *Client) ListTeamIssues(ctx context.Context, teamKey string, limit int) ([]Issue, error) {
	teamKey = strings.ToUpper(strings.TrimSpace(teamKey))
	if teamKey == "" {
		return nil, fmt.Errorf("linear: empty team key")
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 50 {
		limit = 50
	}
	const q = `
query TeamIssues($team: String!, $first: Int!) {
  issues(
    filter: { team: { key: { eq: $team } } }
    first: $first
    orderBy: updatedAt
  ) {
    nodes {
      id
      identifier
      title
      url
      description
      state { name }
      team { key }
    }
  }
}`
	var data struct {
		Issues struct {
			Nodes []struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
				Team struct {
					Key string `json:"key"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.do(ctx, q, map[string]any{"team": teamKey, "first": limit}, &data); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(data.Issues.Nodes))
	for _, n := range data.Issues.Nodes {
		out = append(out, Issue{
			ID:          n.ID,
			Identifier:  n.Identifier,
			Title:       n.Title,
			URL:         n.URL,
			State:       n.State.Name,
			TeamKey:     n.Team.Key,
			Description: n.Description,
		})
	}
	return out, nil
}

func splitIdentifier(id string) (team string, number int, ok bool) {
	id = strings.TrimSpace(id)
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	team = strings.ToUpper(parts[0])
	var n int
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	if team == "" || n <= 0 {
		return "", 0, false
	}
	return team, n, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
