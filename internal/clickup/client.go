// Package clickup is a thin REST client for ClickUp task resolve (L1).
package clickup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBase = "https://api.clickup.com"

// maxResponseBody caps ClickUp response reads. List-task payloads include
// custom fields and descriptions for up to 100 tasks and routinely exceed 1 MiB
// on active lists (observed ~1.8 MiB on 2br-api); silent truncation used to
// surface as "decode: unexpected end of JSON input".
const maxResponseBody = 8 << 20 // 8 MiB

// Client talks to ClickUp REST with a personal API token.
type Client struct {
	APIKey string
	Base   string
	HTTP   *http.Client
}

// New returns a client for the given API key.
func New(apiKey string) *Client {
	return &Client{
		APIKey: strings.TrimSpace(apiKey),
		Base:   defaultBase,
		HTTP:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Task is a resolved ClickUp task snapshot.
type Task struct {
	ID          string
	CustomID    string
	Name        string
	URL         string
	Status      string
	Description string
	ListID      string
	WorkspaceID string
	// WorkState is set by the web layer (not ClickUp): "FIXING" when a
	// non-terminal Grok session binds this task with Fixes.
	WorkState string
}

// GetOpts controls GetTask resolution.
type GetOpts struct {
	// CustomTaskIDs resolves taskID as a custom id (PREFIX-N); requires WorkspaceID.
	CustomTaskIDs bool
	WorkspaceID   string
	Markdown      bool
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, dest any) error {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("clickup: missing API key")
	}
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = defaultBase
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read one byte past the cap so truncation is detectable (LimitReader alone
	// returns a valid prefix and json.Unmarshal fails with a confusing EOF).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxResponseBody {
		return fmt.Errorf("clickup: response too large (>%d bytes)", maxResponseBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("clickup: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("clickup: decode: %w", err)
	}
	return nil
}

// GetTask resolves a task by native id, or by custom id when opts.CustomTaskIDs.
func (c *Client) GetTask(ctx context.Context, taskID string, opts GetOpts) (Task, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Task{}, fmt.Errorf("clickup: empty task id")
	}
	q := url.Values{}
	if opts.Markdown {
		q.Set("include_markdown_description", "true")
	}
	if opts.CustomTaskIDs {
		ws := strings.TrimSpace(opts.WorkspaceID)
		if ws == "" {
			return Task{}, fmt.Errorf("clickup: workspaceId required for custom task ids")
		}
		q.Set("custom_task_ids", "true")
		q.Set("team_id", ws)
	}
	var raw taskJSON
	path := "/api/v2/task/" + url.PathEscape(taskID)
	if err := c.do(ctx, http.MethodGet, path, q, &raw); err != nil {
		return Task{}, err
	}
	return raw.toTask(), nil
}

// ListTasks returns recent tasks for a list (ordered by updated, including closed).
func (c *Client) ListTasks(ctx context.Context, listID string, limit int) ([]Task, error) {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return nil, fmt.Errorf("clickup: empty list id")
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 50 {
		limit = 50
	}
	q := url.Values{}
	q.Set("order_by", "updated")
	q.Set("include_closed", "true")
	// page size; ClickUp max is 100; we cap client-side after fetch
	q.Set("page", "0")

	var raw struct {
		Tasks []taskJSON `json:"tasks"`
	}
	path := "/api/v2/list/" + url.PathEscape(listID) + "/task"
	if err := c.do(ctx, http.MethodGet, path, q, &raw); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(raw.Tasks))
	for _, t := range raw.Tasks {
		out = append(out, t.toTask())
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type taskJSON struct {
	ID                   string `json:"id"`
	CustomID             string `json:"custom_id"`
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	Description          string `json:"description"`
	TextContent          string `json:"text_content"`
	MarkdownDescription  string `json:"markdown_description"`
	Status               *struct {
		Status string `json:"status"`
	} `json:"status"`
	List *struct {
		ID string `json:"id"`
	} `json:"list"`
	TeamID string `json:"team_id"`
}

func (t taskJSON) toTask() Task {
	out := Task{
		ID:          strings.TrimSpace(t.ID),
		CustomID:    strings.TrimSpace(t.CustomID),
		Name:        strings.TrimSpace(t.Name),
		URL:         strings.TrimSpace(t.URL),
		ListID:      "",
		WorkspaceID: strings.TrimSpace(t.TeamID),
	}
	if t.Status != nil {
		out.Status = strings.TrimSpace(t.Status.Status)
	}
	if t.List != nil {
		out.ListID = strings.TrimSpace(t.List.ID)
	}
	desc := strings.TrimSpace(t.MarkdownDescription)
	if desc == "" {
		desc = strings.TrimSpace(t.Description)
	}
	if desc == "" {
		desc = strings.TrimSpace(t.TextContent)
	}
	out.Description = desc
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// LooksLikeCustomID reports whether s is PREFIX-N shape (letters then digits).
func LooksLikeCustomID(s string) bool {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, r := range parts[0] {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return false
	}
	// first char of prefix should be a letter
	r0 := rune(parts[0][0])
	return (r0 >= 'A' && r0 <= 'Z') || (r0 >= 'a' && r0 <= 'z')
}
