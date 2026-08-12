package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	driveAPIBase    = "https://www.googleapis.com/drive/v3"
	driveUploadBase = "https://www.googleapis.com/upload/drive/v3"
	folderMIME      = "application/vnd.google-apps.folder"
	listPageSize    = 200
	// Multipart boundary used for create/update uploads (stable for fakes).
	multipartBoundary = "grokwork_boundary"
)

// Client is a thin Drive REST v3 client.
type Client struct {
	// Auth mints Bearer tokens. Required.
	Auth TokenSource
	// HTTP is used for metadata/list/delete/token-sized calls. Nil → 30s default.
	HTTP *http.Client
	// MediaHTTP is used for upload/download. Nil → 120s default (or HTTP if set with longer timeout).
	MediaHTTP *http.Client
	// APIBase overrides the metadata host (tests). Empty → production.
	APIBase string
	// UploadBase overrides the upload host (tests). Empty → production.
	UploadBase string
}

func (c *Client) http() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) mediaHTTP() *http.Client {
	if c != nil && c.MediaHTTP != nil {
		return c.MediaHTTP
	}
	// Prefer the shared client when tests inject one RoundTripper for everything.
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func (c *Client) apiBase() string {
	if c != nil && strings.TrimSpace(c.APIBase) != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return driveAPIBase
}

func (c *Client) uploadBase() string {
	if c != nil && strings.TrimSpace(c.UploadBase) != "" {
		return strings.TrimRight(c.UploadBase, "/")
	}
	return driveUploadBase
}

// fileMeta is the subset of Drive file fields we use.
type fileMeta struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         string `json:"size"` // Drive returns size as string
	ModifiedTime string `json:"modifiedTime"`
	CreatedTime  string `json:"createdTime"`
}

func (f fileMeta) isFolder() bool {
	return f.MimeType == folderMIME
}

func (f fileMeta) sizeInt() int64 {
	if f.Size == "" {
		return 0
	}
	var n int64
	for _, r := range f.Size {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func (f fileMeta) modified() time.Time {
	if f.ModifiedTime == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, f.ModifiedTime)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, f.ModifiedTime)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func (f fileMeta) created() time.Time {
	if f.CreatedTime == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, f.CreatedTime)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, f.CreatedTime)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func isGoogleNativeMIME(m string) bool {
	return strings.HasPrefix(m, "application/vnd.google-apps.") && m != folderMIME
}

// doJSON issues an authorized request and decodes JSON into dest when non-nil.
// For 204 responses dest is ignored. Returns status code.
func (c *Client) doJSON(ctx context.Context, method, fullURL string, body any, dest any) (int, error) {
	if c == nil || c.Auth == nil {
		return 0, fmt.Errorf("drive: client not configured")
	}
	token, err := c.Auth.Token(ctx)
	if err != nil {
		return 0, err
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, mapDriveError(method, resp.StatusCode, raw)
	}
	if dest != nil && len(raw) > 0 && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, dest); err != nil {
			return resp.StatusCode, fmt.Errorf("drive: decode: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func mapDriveError(method string, status int, body []byte) error {
	reason := ""
	var errBody struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errBody) == nil && errBody.Error.Message != "" {
		reason = errBody.Error.Message
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("drive: permission denied (check SA has access to the Shared Drive / folder)")
	case http.StatusNotFound:
		return fmt.Errorf("drive: not found")
	case http.StatusTooManyRequests:
		return fmt.Errorf("drive: rate limited, retry later")
	default:
		if reason == "" {
			reason = fmt.Sprintf("status %d", status)
		}
		return fmt.Errorf("drive: %s: %d %s", method, status, reason)
	}
}

// escapeDriveQuery escapes a value for use inside single-quoted Drive q= strings.
func escapeDriveQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

// listChildren lists one level under parentID. nameFilter/foldersOnly narrow the q=.
// orderBy defaults to "folder,name" when empty.
func (c *Client) listChildren(ctx context.Context, parentID, nameFilter string, foldersOnly bool, orderBy string) ([]fileMeta, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil, fmt.Errorf("drive: parent id is required")
	}
	q := fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveQuery(parentID))
	if nameFilter != "" {
		q += fmt.Sprintf(" and name = '%s'", escapeDriveQuery(nameFilter))
	}
	if foldersOnly {
		q += fmt.Sprintf(" and mimeType = '%s'", folderMIME)
	}
	if orderBy == "" {
		orderBy = "folder,name"
	}
	var out []fileMeta
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("q", q)
		params.Set("pageSize", fmt.Sprintf("%d", listPageSize))
		params.Set("fields", "nextPageToken,files(id,name,mimeType,size,modifiedTime,createdTime)")
		params.Set("supportsAllDrives", "true")
		params.Set("includeItemsFromAllDrives", "true")
		params.Set("orderBy", orderBy)
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		u := c.apiBase() + "/files?" + params.Encode()
		var page struct {
			Files         []fileMeta `json:"files"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if _, err := c.doJSON(ctx, http.MethodGet, u, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Files...)
		// Cap collection slightly past UI limit so callers can detect clipping.
		if len(out) >= listPageSize+1 || page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// findChildByName resolves a single child by name under parent.
// foldersOnly restricts to folders. Ambiguous (>1) is an error for user paths.
func (c *Client) findChildByName(ctx context.Context, parentID, name string, foldersOnly bool) (fileMeta, bool, error) {
	matches, err := c.listChildren(ctx, parentID, name, foldersOnly, "folder,name")
	if err != nil {
		return fileMeta{}, false, err
	}
	switch len(matches) {
	case 0:
		return fileMeta{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return fileMeta{}, false, fmt.Errorf("drive: ambiguous name %q under parent", name)
	}
}

// ensureIsolationFolder find-or-creates the isolation child with race recovery (K8b).
func (c *Client) ensureIsolationFolder(ctx context.Context, parentID, segment string) (string, error) {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return "", fmt.Errorf("drive: isolation segment is empty")
	}
	matches, err := c.listChildren(ctx, parentID, segment, true, "createdTime")
	if err != nil {
		return "", err
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) == 0 {
		created, err := c.createFolder(ctx, parentID, segment)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "[info] drive: created isolation folder %q under %s\n", segment, parentID)
		// Re-list — concurrent creators may have raced.
		matches, err = c.listChildren(ctx, parentID, segment, true, "createdTime")
		if err != nil {
			// Fall back to the one we just created.
			return created.ID, nil
		}
		if len(matches) == 0 {
			return created.ID, nil
		}
	}
	// len >= 1 (possibly after race): oldest wins; delete younger duplicates.
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		ci, cj := matches[i].created(), matches[j].created()
		// Missing createdTime last.
		if ci.IsZero() && !cj.IsZero() {
			return false
		}
		if !ci.IsZero() && cj.IsZero() {
			return true
		}
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		return matches[i].ID < matches[j].ID
	})
	winner := matches[0]
	for _, m := range matches[1:] {
		if err := c.deleteByID(ctx, m.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] drive: removed duplicate isolation folder id=%s name=%q: %v\n", m.ID, m.Name, err)
		} else {
			fmt.Fprintf(os.Stderr, "[warn] drive: removed duplicate isolation folder id=%s name=%q\n", m.ID, m.Name)
		}
	}
	return winner.ID, nil
}

func (c *Client) createFolder(ctx context.Context, parentID, name string) (fileMeta, error) {
	params := url.Values{}
	params.Set("supportsAllDrives", "true")
	params.Set("fields", "id,name,mimeType,createdTime")
	u := c.apiBase() + "/files?" + params.Encode()
	body := map[string]any{
		"name":     name,
		"mimeType": folderMIME,
		"parents":  []string{parentID},
	}
	var out fileMeta
	if _, err := c.doJSON(ctx, http.MethodPost, u, body, &out); err != nil {
		return fileMeta{}, err
	}
	if out.ID == "" {
		return fileMeta{}, fmt.Errorf("drive: create folder returned empty id")
	}
	return out, nil
}

func (c *Client) deleteByID(ctx context.Context, id string) error {
	params := url.Values{}
	params.Set("supportsAllDrives", "true")
	u := c.apiBase() + "/files/" + url.PathEscape(id) + "?" + params.Encode()
	_, err := c.doJSON(ctx, http.MethodDelete, u, nil, nil)
	return err
}

func (c *Client) getMeta(ctx context.Context, id string) (fileMeta, error) {
	params := url.Values{}
	params.Set("fields", "id,name,mimeType,size,modifiedTime,createdTime")
	params.Set("supportsAllDrives", "true")
	u := c.apiBase() + "/files/" + url.PathEscape(id) + "?" + params.Encode()
	var out fileMeta
	if _, err := c.doJSON(ctx, http.MethodGet, u, nil, &out); err != nil {
		return fileMeta{}, err
	}
	return out, nil
}

// ensureEffectiveRoot returns the folder id that user paths are relative to.
func (c *Client) ensureEffectiveRoot(ctx context.Context, folderID, isolationSegment string) (string, error) {
	folderID = strings.TrimSpace(folderID)
	if folderID == "" {
		return "", fmt.Errorf("drive: folder id is required")
	}
	seg := strings.TrimSpace(isolationSegment)
	if seg == "" {
		return folderID, nil
	}
	return c.ensureIsolationFolder(ctx, folderID, seg)
}
