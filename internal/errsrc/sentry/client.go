// Package sentry is a thin REST client for Sentry issue list/get.
package sentry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
)

const (
	defaultBaseURL = "https://sentry.io"
	maxBody        = 1 << 20
)

// Client talks to Sentry REST. Org and Project come from operator config only.
type Client struct {
	Token   string
	Org     string
	Project string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client. baseURL empty → https://sentry.io.
func New(token, org, project, baseURL string) *Client {
	return &Client{
		Token:   strings.TrimSpace(token),
		Org:     strings.TrimSpace(org),
		Project: strings.TrimSpace(project),
		BaseURL: strings.TrimSpace(baseURL),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) base() (string, error) {
	raw := strings.TrimSpace(c.BaseURL)
	if raw == "" {
		raw = defaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("sentry: baseURL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" || strings.Contains(u.Host, `\`) {
		return "", fmt.Errorf("sentry: invalid baseURL")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, dest any) (http.Header, error) {
	return c.doJSON(ctx, method, path, query, nil, dest)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, dest any) (http.Header, error) {
	if c == nil || strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("sentry: missing token")
	}
	base, err := c.base()
	if err != nil {
		return nil, err
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, fmt.Errorf("sentry: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if dest == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return resp.Header, fmt.Errorf("sentry: decode: %w", err)
	}
	return resp.Header, nil
}

type issueJSON struct {
	ID        string          `json:"id"`
	ShortID   string          `json:"shortId"`
	Title     string          `json:"title"`
	Culprit   string          `json:"culprit"`
	Permalink string          `json:"permalink"`
	Status    string          `json:"status"`
	Level     string          `json:"level"`
	Count     json.RawMessage `json:"count"`
	UserCount int64           `json:"userCount"`
	FirstSeen time.Time       `json:"firstSeen"`
	LastSeen  time.Time       `json:"lastSeen"`
	Metadata  struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"metadata"`
	Project struct {
		Slug string `json:"slug"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
}

func parseCount(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, _ = strconv.ParseInt(s, 10, 64)
		return n
	}
	return 0
}

func (iss issueJSON) toGroup(providerProject string) errsrc.Group {
	return errsrc.Group{
		Provider:  errsrc.ProviderSentry,
		ID:        iss.ID,
		ShortID:   iss.ShortID,
		Title:     iss.Title,
		Culprit:   iss.Culprit,
		Status:    iss.Status,
		Level:     iss.Level,
		Count:     parseCount(iss.Count),
		UserCount: iss.UserCount,
		FirstSeen: iss.FirstSeen,
		LastSeen:  iss.LastSeen,
		URL:       iss.Permalink,
		Resource:  iss.Project.Slug,
	}
}

// List returns project issues. org/project from the client config only.
func (c *Client) List(ctx context.Context, q errsrc.ListQuery) (errsrc.ListResult, error) {
	if c == nil || c.Org == "" || c.Project == "" {
		return errsrc.ListResult{}, fmt.Errorf("sentry: org and project required")
	}
	query := url.Values{}
	status := strings.TrimSpace(q.Status)
	if status == "" {
		status = "is:unresolved"
	}
	query.Set("query", status)
	if q.Sort != "" {
		query.Set("sort", q.Sort)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	query.Set("limit", strconv.Itoa(limit))
	if q.Cursor != "" {
		query.Set("cursor", q.Cursor)
	}
	var issues []issueJSON
	hdr, err := c.do(ctx, http.MethodGet, "/api/0/projects/"+url.PathEscape(c.Org)+"/"+url.PathEscape(c.Project)+"/issues/", query, &issues)
	if err != nil {
		return errsrc.ListResult{}, err
	}
	out := errsrc.ListResult{Groups: make([]errsrc.Group, 0, len(issues))}
	for _, iss := range issues {
		out.Groups = append(out.Groups, iss.toGroup(c.Project))
	}
	out.NextCursor = nextCursor(hdr)
	return out, nil
}

func nextCursor(h http.Header) string {
	if h == nil {
		return ""
	}
	// Link: <...>; rel="next"; results="true"; cursor="..."
	for part := range strings.SplitSeq(h.Get("Link"), ",") {
		if !strings.Contains(part, `rel="next"`) || strings.Contains(part, `results="false"`) {
			continue
		}
		if i := strings.Index(part, "cursor="); i >= 0 {
			rest := part[i+len("cursor="):]
			rest = strings.Trim(rest, `" >;`)
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

// Get fetches one issue via the org-scoped route. id may be numeric or shortId.
func (c *Client) Get(ctx context.Context, id string) (errsrc.GroupDetail, string, error) {
	id = strings.TrimSpace(id)
	if c == nil || c.Org == "" {
		return errsrc.GroupDetail{}, "", fmt.Errorf("sentry: org required")
	}
	if id == "" {
		return errsrc.GroupDetail{}, "", fmt.Errorf("sentry: id required")
	}
	if looksLikeShortID(id) {
		resolved, err := c.resolveShortID(ctx, id)
		if err != nil {
			return errsrc.GroupDetail{}, "", err
		}
		id = resolved
	}
	var iss issueJSON
	_, err := c.do(ctx, http.MethodGet, "/api/0/organizations/"+url.PathEscape(c.Org)+"/issues/"+url.PathEscape(id)+"/", nil, &iss)
	if err != nil {
		return errsrc.GroupDetail{}, "", err
	}
	g := iss.toGroup(c.Project)
	detail := errsrc.GroupDetail{Group: g}
	if ev, evErr := c.latestEvent(ctx, iss.ID); evErr == nil {
		detail.Sample = errsrc.CapSample(ev.Sample)
		detail.Recent = ev.Recent
		if detail.Culprit == "" {
			detail.Culprit = ev.Culprit
		}
	}
	return detail, iss.Project.Slug, nil
}

// UpdateStatus sets an issue to resolved or unresolved (open). id may be numeric or shortId.
// The PUT is org-scoped; callers must post-check project slug before calling when
// containment matters (the web handler Gets first).
func (c *Client) UpdateStatus(ctx context.Context, id, status string) error {
	id = strings.TrimSpace(id)
	if c == nil || c.Org == "" {
		return fmt.Errorf("sentry: org required")
	}
	if id == "" {
		return fmt.Errorf("sentry: id required")
	}
	native := ""
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved":
		native = "resolved"
	case "open":
		native = "unresolved"
	default:
		return fmt.Errorf("sentry: status must be resolved or open")
	}
	if looksLikeShortID(id) {
		resolved, err := c.resolveShortID(ctx, id)
		if err != nil {
			return err
		}
		id = resolved
	}
	body := map[string]string{"status": native}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/0/organizations/"+url.PathEscape(c.Org)+"/issues/"+url.PathEscape(id)+"/", nil, body, nil)
	return err
}

func looksLikeShortID(id string) bool {
	// APP-1A: letters, dash, then alnum — not a pure numeric id.
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return strings.Contains(id, "-")
		}
	}
	return false
}

func (c *Client) resolveShortID(ctx context.Context, shortID string) (string, error) {
	var wrap struct {
		GroupID string `json:"groupId"`
		Issue   struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	_, err := c.do(ctx, http.MethodGet, "/api/0/organizations/"+url.PathEscape(c.Org)+"/shortids/"+url.PathEscape(shortID)+"/", nil, &wrap)
	if err != nil {
		return "", err
	}
	if wrap.Issue.ID != "" {
		return wrap.Issue.ID, nil
	}
	if wrap.GroupID != "" {
		return wrap.GroupID, nil
	}
	return "", fmt.Errorf("sentry: short id %s not found", shortID)
}

type eventJSON struct {
	DateCreated time.Time `json:"dateCreated"`
	Culprit     string    `json:"culprit"`
	Entries     []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"entries"`
}

func (c *Client) latestEvent(ctx context.Context, issueID string) (errsrc.GroupDetail, error) {
	var ev eventJSON
	_, err := c.do(ctx, http.MethodGet, "/api/0/organizations/"+url.PathEscape(c.Org)+"/issues/"+url.PathEscape(issueID)+"/events/latest/", nil, &ev)
	if err != nil {
		return errsrc.GroupDetail{}, err
	}
	sample := extractStack(ev)
	return errsrc.GroupDetail{
		Sample: sample,
		Recent: []errsrc.Event{{Timestamp: ev.DateCreated, Message: errsrc.CapSample(sample), Culprit: ev.Culprit}},
		Group:  errsrc.Group{Culprit: ev.Culprit},
	}, nil
}

func extractStack(ev eventJSON) string {
	var b strings.Builder
	for _, e := range ev.Entries {
		if e.Type != "exception" {
			continue
		}
		var data struct {
			Values []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
				Stack struct {
					Frames []struct {
						Function string `json:"function"`
						Filename string `json:"filename"`
						LineNo   int    `json:"lineNo"`
					} `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		}
		if json.Unmarshal(e.Data, &data) != nil {
			continue
		}
		for _, v := range data.Values {
			fmt.Fprintf(&b, "%s: %s\n", v.Type, v.Value)
			for _, f := range v.Stack.Frames {
				fmt.Fprintf(&b, "  %s %s:%d\n", f.Function, f.Filename, f.LineNo)
			}
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// MatchProject reports whether the issue's project slug equals the configured one.
func MatchProject(gotSlug, configured string) bool {
	return strings.EqualFold(strings.TrimSpace(gotSlug), strings.TrimSpace(configured))
}
