// Package deploys is a thin HTTP client for deploys.app error.list / error.get,
// plus a CLI wrapper that mints a 1-year token via `deploys me generate-token`.
// It is not grokwork's own deploy pipeline (see internal/deploy).
package deploys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/errsrc"
)

const (
	defaultEndpoint = "https://api.deploys.app/"
	headerChannel   = "X-Deploys-Channel"
	channelValue    = "grokwork"
	maxBody         = 1 << 20
	httpTimeout     = 15 * time.Second
)

// Options constructs a Client. Endpoint is operator-only (tests override it).
type Options struct {
	Token     string
	BasicUser string
	BasicPass string
	Endpoint  string
	HTTP      *http.Client
}

// Client talks to deploys.app POST /{action} JSON {ok, result, error}.
type Client struct {
	Token     string
	BasicUser string
	BasicPass string
	Endpoint  string
	HTTP      *http.Client
}

// New returns a client. Empty auth still builds; List/Get fail at invoke.
func New(opts Options) *Client {
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	} else if httpClient.Timeout == 0 {
		cp := *httpClient
		cp.Timeout = httpTimeout
		httpClient = &cp
	}
	return &Client{
		Token:     strings.TrimSpace(opts.Token),
		BasicUser: strings.TrimSpace(opts.BasicUser),
		BasicPass: opts.BasicPass,
		Endpoint:  strings.TrimSpace(opts.Endpoint),
		HTTP:      httpClient,
	}
}

// ListReq is error.list. Project is required; location+name narrow one deployment.
type ListReq struct {
	Project  string `json:"project"`
	Location string `json:"location,omitempty"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Sort     string `json:"sort,omitempty"`
}

// GetReq is error.get. All four fields are required — no default fill.
type GetReq struct {
	Project  string `json:"project"`
	Location string `json:"location"`
	Name     string `json:"name"`
	ID       string `json:"id"`
}

type listResult struct {
	Issues     []issue `json:"issues"`
	NextCursor string  `json:"nextCursor"`
}

type getResult struct {
	Issue issueDetail `json:"issue"`
}

type issue struct {
	ID          string    `json:"id"`
	Deployment  string    `json:"deployment"`
	Location    string    `json:"location"`
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Count       int64     `json:"count"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	SamplePod   string    `json:"samplePod"`
}

type issueDetail struct {
	issue
	SampleMessage string       `json:"sampleMessage"`
	RecentEvents  []occurrence `json:"recentEvents"`
}

type occurrence struct {
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Object    string    `json:"object"`
	Offset    int       `json:"offset"`
}

// List posts error.list and maps to errsrc.ListResult.
func (c *Client) List(ctx context.Context, in ListReq) (errsrc.ListResult, error) {
	in.Project = strings.TrimSpace(in.Project)
	in.Location = strings.TrimSpace(in.Location)
	in.Name = strings.TrimSpace(in.Name)
	in.Status = strings.TrimSpace(in.Status)
	in.Cursor = strings.TrimSpace(in.Cursor)
	in.Sort = strings.TrimSpace(in.Sort)
	if in.Project == "" {
		return errsrc.ListResult{}, fmt.Errorf("deploys: project required")
	}
	var raw listResult
	if err := c.invoke(ctx, "error.list", in, &raw); err != nil {
		return errsrc.ListResult{}, err
	}
	out := errsrc.ListResult{NextCursor: strings.TrimSpace(raw.NextCursor)}
	out.Groups = make([]errsrc.Group, 0, len(raw.Issues))
	for _, iss := range raw.Issues {
		out.Groups = append(out.Groups, groupFromIssue(in.Project, iss))
	}
	return out, nil
}

// Get posts error.get. Missing location or name fails locally — no default fill.
func (c *Client) Get(ctx context.Context, in GetReq) (errsrc.GroupDetail, error) {
	in.Project = strings.TrimSpace(in.Project)
	in.Location = strings.TrimSpace(in.Location)
	in.Name = strings.TrimSpace(in.Name)
	in.ID = strings.TrimSpace(in.ID)
	if in.Project == "" || in.Location == "" || in.Name == "" || in.ID == "" {
		return errsrc.GroupDetail{}, fmt.Errorf("deploys: project, location, name, and id are required")
	}
	var raw getResult
	if err := c.invoke(ctx, "error.get", in, &raw); err != nil {
		return errsrc.GroupDetail{}, err
	}
	g := groupFromIssue(in.Project, raw.Issue.issue)
	detail := errsrc.GroupDetail{
		Group:  g,
		Sample: errsrc.CapSample(raw.Issue.SampleMessage),
	}
	if n := len(raw.Issue.RecentEvents); n > 0 {
		detail.Recent = make([]errsrc.Event, 0, n)
		for _, ev := range raw.Issue.RecentEvents {
			detail.Recent = append(detail.Recent, errsrc.Event{
				Timestamp: ev.Timestamp,
				Culprit:   ev.Pod,
				Extra:     occurrenceExtra(ev),
			})
		}
	}
	return detail, nil
}

func occurrenceExtra(ev occurrence) string {
	var b strings.Builder
	if ev.Object != "" {
		b.WriteString(ev.Object)
		if ev.Offset != 0 {
			fmt.Fprintf(&b, ":%d", ev.Offset)
		}
	}
	return b.String()
}

func groupFromIssue(project string, iss issue) errsrc.Group {
	return errsrc.Group{
		Provider:    errsrc.ProviderDeploys,
		ID:          iss.ID,
		Title:       iss.Title,
		Culprit:     iss.Kind,
		Status:      iss.Status,
		Count:       iss.Count,
		FirstSeen:   iss.FirstSeen,
		LastSeen:    iss.LastSeen,
		URL:         ConsoleURL(project, iss.Location, iss.Deployment, iss.ID),
		Fingerprint: iss.Fingerprint,
		Location:    iss.Location,
		Resource:    iss.Deployment,
	}
}

func (c *Client) invoke(ctx context.Context, action string, reqBody any, dest any) error {
	if c == nil {
		return fmt.Errorf("deploys: nil client")
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint()+action, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(headerChannel, channelValue)
	c.applyAuth(req)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deploys: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("deploys: decode: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("deploys: %s", decodeAPIError(env.Error))
	}
	if dest == nil {
		return nil
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return fmt.Errorf("deploys: empty result")
	}
	if err := json.Unmarshal(env.Result, dest); err != nil {
		return fmt.Errorf("deploys: decode result: %w", err)
	}
	return nil
}

func (c *Client) applyAuth(req *http.Request) {
	if c.BasicUser != "" && c.BasicPass != "" {
		req.SetBasicAuth(c.BasicUser, c.BasicPass)
		return
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func (c *Client) endpoint() string {
	ep := c.Endpoint
	if ep == "" {
		ep = defaultEndpoint
	}
	if !strings.HasSuffix(ep, "/") {
		ep += "/"
	}
	return ep
}

func decodeAPIError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "request failed"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.Message) != "" {
		return obj.Message
	}
	return strings.TrimSpace(string(raw))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
