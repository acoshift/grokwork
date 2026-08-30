// Package gcperr is a thin REST client for Cloud Error Reporting v1beta1.
// It does not exec gcloud and does not import Cloud SDKs.
package gcperr

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
	defaultAPI = "https://clouderrorreporting.googleapis.com/v1beta1"
	maxBody    = 1 << 20
)

// Client lists and gets Error Reporting groups.
type Client struct {
	ProjectID string
	Service   string
	Tokens    TokenSource
	HTTP      *http.Client
	Endpoint  string // tests
}

func (c *Client) http() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) endpoint() string {
	if c != nil && strings.TrimSpace(c.Endpoint) != "" {
		return strings.TrimRight(c.Endpoint, "/")
	}
	return defaultAPI
}

func (c *Client) get(ctx context.Context, path string, query url.Values, dest any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, dest)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, dest any) error {
	if c == nil || strings.TrimSpace(c.ProjectID) == "" {
		return fmt.Errorf("gcp: projectId required")
	}
	if c.Tokens == nil {
		return fmt.Errorf("gcp: no token source")
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := c.endpoint() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gcp: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if dest == nil {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

type groupStatsResp struct {
	ErrorGroupStats []groupStat `json:"errorGroupStats"`
	NextPageToken   string      `json:"nextPageToken"`
}

type groupStat struct {
	Group struct {
		GroupID          string `json:"groupId"`
		Name             string `json:"name"`
		ResolutionStatus string `json:"resolutionStatus"`
	} `json:"group"`
	Count              string    `json:"count"`
	AffectedUsersCount string    `json:"affectedUsersCount"`
	FirstSeenTime      time.Time `json:"firstSeenTime"`
	LastSeenTime       time.Time `json:"lastSeenTime"`
	Representative     struct {
		Message string `json:"message"`
		Service struct {
			Service string `json:"service"`
		} `json:"serviceContext"`
	} `json:"representative"`
}

func parseInt64String(s string) int64 {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (g groupStat) toGroup(projectID string) errsrc.Group {
	title := g.Representative.Message
	if i := strings.Index(title, "\n"); i > 0 {
		title = title[:i]
	}
	permalink := ""
	if id := strings.TrimSpace(g.Group.GroupID); id != "" && projectID != "" {
		permalink = "https://console.cloud.google.com/errors/detail/" + url.PathEscape(id) + "?project=" + url.QueryEscape(projectID)
	}
	return errsrc.Group{
		Provider:  errsrc.ProviderGCP,
		ID:        g.Group.GroupID,
		Title:     title,
		Culprit:   g.Representative.Service.Service,
		Status:    gcpStatus(g.Group.ResolutionStatus),
		Count:     parseInt64String(g.Count),
		UserCount: parseInt64String(g.AffectedUsersCount),
		FirstSeen: g.FirstSeenTime,
		LastSeen:  g.LastSeenTime,
		Resource:  g.Representative.Service.Service,
		URL:       permalink,
	}
}

func gcpStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "RESOLVED":
		return "resolved"
	case "ACKNOWLEDGED":
		return "acknowledged"
	case "MUTED":
		return "muted"
	case "OPEN", "RESOLUTION_STATUS_UNSPECIFIED", "":
		return "open"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func gcpResolutionStatus(status string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved":
		return "RESOLVED", nil
	case "open":
		return "OPEN", nil
	default:
		return "", fmt.Errorf("gcp: status must be resolved or open")
	}
}

func normalizePeriod(p string) string {
	switch strings.TrimSpace(p) {
	case "PERIOD_1_HOUR", "PERIOD_6_HOURS", "PERIOD_1_DAY", "PERIOD_1_WEEK", "PERIOD_30_DAYS":
		return strings.TrimSpace(p)
	default:
		return "PERIOD_1_DAY"
	}
}

// List returns groupStats for the project.
func (c *Client) List(ctx context.Context, q errsrc.ListQuery) (errsrc.ListResult, error) {
	period := normalizePeriod(q.TimeRange)
	query := url.Values{}
	query.Set("timeRange.period", period)
	order := strings.TrimSpace(q.Sort)
	if order == "" {
		order = "COUNT_DESC"
	}
	query.Set("order", order)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	query.Set("pageSize", strconv.Itoa(limit))
	if q.Cursor != "" {
		query.Set("pageToken", q.Cursor)
	}
	svc := strings.TrimSpace(q.Service)
	if svc == "" {
		svc = strings.TrimSpace(c.Service)
	}
	if svc != "" {
		query.Set("serviceFilter.service", svc)
	}
	var resp groupStatsResp
	if err := c.get(ctx, "/projects/"+url.PathEscape(c.ProjectID)+"/groupStats", query, &resp); err != nil {
		return errsrc.ListResult{}, err
	}
	out := errsrc.ListResult{NextCursor: resp.NextPageToken}
	for _, g := range resp.ErrorGroupStats {
		out.Groups = append(out.Groups, g.toGroup(c.ProjectID))
	}
	return out, nil
}

type eventsResp struct {
	ErrorEvents []struct {
		EventTime time.Time `json:"eventTime"`
		Message   string    `json:"message"`
		Service   struct {
			Service string `json:"service"`
		} `json:"serviceContext"`
	} `json:"errorEvents"`
}

// Get lists groupStats filtered by groupId plus events with the same period.
func (c *Client) Get(ctx context.Context, groupID, period string) (errsrc.GroupDetail, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return errsrc.GroupDetail{}, fmt.Errorf("gcp: group id required")
	}
	if strings.Contains(groupID, "/") {
		return errsrc.GroupDetail{}, fmt.Errorf("gcp: group id must be a single path segment")
	}
	period = normalizePeriod(period)
	query := url.Values{}
	query.Set("groupId", groupID)
	query.Set("timeRange.period", period)
	query.Set("pageSize", "1")
	var stats groupStatsResp
	if err := c.get(ctx, "/projects/"+url.PathEscape(c.ProjectID)+"/groupStats", query, &stats); err != nil {
		return errsrc.GroupDetail{}, err
	}
	if len(stats.ErrorGroupStats) == 0 {
		return errsrc.GroupDetail{}, fmt.Errorf("gcp: group %s not found", groupID)
	}
	g := stats.ErrorGroupStats[0].toGroup(c.ProjectID)
	evQ := url.Values{}
	evQ.Set("groupId", groupID)
	evQ.Set("timeRange.period", period)
	evQ.Set("pageSize", "5")
	var evs eventsResp
	if err := c.get(ctx, "/projects/"+url.PathEscape(c.ProjectID)+"/events", evQ, &evs); err != nil {
		return errsrc.GroupDetail{Group: g}, nil
	}
	detail := errsrc.GroupDetail{Group: g}
	for i, ev := range evs.ErrorEvents {
		msg := errsrc.CapSample(ev.Message)
		if i == 0 {
			detail.Sample = msg
		}
		detail.Recent = append(detail.Recent, errsrc.Event{
			Timestamp: ev.EventTime,
			Message:   msg,
			Culprit:   ev.Service.Service,
		})
	}
	return detail, nil
}

// UpdateResolution GETs the ErrorGroup then PUTs it with resolutionStatus set.
// A PUT of only groupId+status would wipe trackingIssues (the API is a full replace).
func (c *Client) UpdateResolution(ctx context.Context, groupID, status string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return fmt.Errorf("gcp: group id required")
	}
	if strings.Contains(groupID, "/") {
		return fmt.Errorf("gcp: group id must be a single path segment")
	}
	native, err := gcpResolutionStatus(status)
	if err != nil {
		return err
	}
	path := "/projects/" + url.PathEscape(c.ProjectID) + "/groups/" + url.PathEscape(groupID)
	var g map[string]any
	if err := c.get(ctx, path, nil, &g); err != nil {
		return err
	}
	if len(g) == 0 {
		return fmt.Errorf("gcp: group %s not found", groupID)
	}
	g["resolutionStatus"] = native
	return c.do(ctx, http.MethodPut, path, nil, g, nil)
}
