package gdrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one listing / describe row (mirrors filestore.Entry without importing it).
type Entry struct {
	Name        string
	IsDir       bool
	Size        int64
	Updated     time.Time
	ContentType string
}

// Target is the resolved Drive identity for one operation.
type Target struct {
	FolderID         string
	IsolationSegment string
	CredentialsFile  string // unused by Client (auth is on Client.Auth); kept for parity
}

// List returns one level of children under subPath (relative to effective root).
// At most listPageSize entries are returned; if more existed, the remainder is dropped
// (caller may treat len==200 as potentially clipped — web layer clips at 200).
func (c *Client) List(ctx context.Context, t Target, subPath string) ([]Entry, error) {
	root, err := c.ensureEffectiveRoot(ctx, t.FolderID, t.IsolationSegment)
	if err != nil {
		return nil, err
	}
	parentID, err := c.resolveDirPath(ctx, root, subPath)
	if err != nil {
		return nil, err
	}
	files, err := c.listChildren(ctx, parentID, "", false, "folder,name")
	if err != nil {
		return nil, err
	}
	if len(files) > listPageSize {
		files = files[:listPageSize]
	}
	out := make([]Entry, 0, len(files))
	for _, f := range files {
		out = append(out, Entry{
			Name:        f.Name,
			IsDir:       f.isFolder(),
			Size:        f.sizeInt(),
			Updated:     f.modified(),
			ContentType: f.MimeType,
		})
	}
	return out, nil
}

// Describe returns metadata for a single non-folder object path.
// exists=true only for a single non-folder leaf; ambiguous → error.
func (c *Client) Describe(ctx context.Context, t Target, object string) (Entry, bool, error) {
	object = strings.TrimSpace(object)
	if object == "" {
		return Entry{}, false, fmt.Errorf("drive: object path is required")
	}
	root, err := c.ensureEffectiveRoot(ctx, t.FolderID, t.IsolationSegment)
	if err != nil {
		return Entry{}, false, err
	}
	parentID, leaf, err := c.resolveParentAndLeaf(ctx, root, object, false)
	if err != nil {
		return Entry{}, false, err
	}
	matches, err := c.listChildren(ctx, parentID, leaf, false, "folder,name")
	if err != nil {
		return Entry{}, false, err
	}
	var files []fileMeta
	for _, m := range matches {
		if !m.isFolder() {
			files = append(files, m)
		}
	}
	switch len(files) {
	case 0:
		return Entry{}, false, nil
	case 1:
		f := files[0]
		return Entry{
			Name:        f.Name,
			IsDir:       false,
			Size:        f.sizeInt(),
			Updated:     f.modified(),
			ContentType: f.MimeType,
		}, true, nil
	default:
		return Entry{}, false, fmt.Errorf("drive: ambiguous name %q under parent", leaf)
	}
}

// Upload creates or overwrites a file at object (relative path).
// Intermediate folders are auto-created (K13). Overwrite updates by id (K15).
func (c *Client) Upload(ctx context.Context, localPath string, t Target, object string, overwrite bool) error {
	object = strings.TrimSpace(object)
	if object == "" {
		return fmt.Errorf("drive: object path is required")
	}
	root, err := c.ensureEffectiveRoot(ctx, t.FolderID, t.IsolationSegment)
	if err != nil {
		return err
	}
	parentID, leaf, err := c.resolveParentAndLeaf(ctx, root, object, true /* create parents */)
	if err != nil {
		return err
	}
	matches, err := c.listChildren(ctx, parentID, leaf, false, "folder,name")
	if err != nil {
		return err
	}
	var files, folders []fileMeta
	for _, m := range matches {
		if m.isFolder() {
			folders = append(folders, m)
		} else {
			files = append(files, m)
		}
	}
	if len(files) > 1 {
		return fmt.Errorf("drive: ambiguous name %q under parent", leaf)
	}
	contentType := contentTypeFor(leaf, localPath)
	if len(files) == 1 {
		if !overwrite {
			return fmt.Errorf("object %q already exists (tick Overwrite to replace)", object)
		}
		return c.updateFileMedia(ctx, files[0].ID, localPath, contentType)
	}
	if len(folders) > 0 {
		return fmt.Errorf("drive: name %q is a folder", leaf)
	}
	return c.createFileMultipart(ctx, parentID, leaf, localPath, contentType)
}

// Download writes the object bytes to destPath. Refuses Google-native mime types (K9).
func (c *Client) Download(ctx context.Context, t Target, object, destPath string) error {
	object = strings.TrimSpace(object)
	if object == "" {
		return fmt.Errorf("drive: object path is required")
	}
	root, err := c.ensureEffectiveRoot(ctx, t.FolderID, t.IsolationSegment)
	if err != nil {
		return err
	}
	parentID, leaf, err := c.resolveParentAndLeaf(ctx, root, object, false)
	if err != nil {
		return err
	}
	matches, err := c.listChildren(ctx, parentID, leaf, false, "folder,name")
	if err != nil {
		return err
	}
	var files []fileMeta
	for _, m := range matches {
		if !m.isFolder() {
			files = append(files, m)
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("drive: not found")
	}
	if len(files) > 1 {
		return fmt.Errorf("drive: ambiguous name %q under parent", leaf)
	}
	f := files[0]
	if isGoogleNativeMIME(f.MimeType) {
		return fmt.Errorf("download of Google-native file %q is not supported (export later); mime=%s", leaf, f.MimeType)
	}
	return c.downloadMedia(ctx, f.ID, destPath)
}

// Delete permanently deletes a single non-folder object. Refuses folders (K14).
func (c *Client) Delete(ctx context.Context, t Target, object string) error {
	object = strings.TrimSpace(object)
	if object == "" {
		return fmt.Errorf("drive: object path is required")
	}
	root, err := c.ensureEffectiveRoot(ctx, t.FolderID, t.IsolationSegment)
	if err != nil {
		return err
	}
	parentID, leaf, err := c.resolveParentAndLeaf(ctx, root, object, false)
	if err != nil {
		return err
	}
	matches, err := c.listChildren(ctx, parentID, leaf, false, "folder,name")
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("drive: not found")
	}
	if len(matches) > 1 {
		// Prefer single non-folder if only one file among them.
		var files []fileMeta
		for _, m := range matches {
			if !m.isFolder() {
				files = append(files, m)
			}
		}
		if len(files) != 1 {
			return fmt.Errorf("drive: ambiguous name %q under parent", leaf)
		}
		return c.deleteByID(ctx, files[0].ID)
	}
	m := matches[0]
	if m.isFolder() {
		return fmt.Errorf("refusing delete: path is a folder (not a single file)")
	}
	// Google-native mime still allowed for delete.
	return c.deleteByID(ctx, m.ID)
}

// resolveDirPath walks every segment of relPath as folders (no create).
func (c *Client) resolveDirPath(ctx context.Context, rootID, relPath string) (string, error) {
	relPath = strings.Trim(strings.TrimSpace(relPath), "/")
	if relPath == "" {
		return rootID, nil
	}
	cur := rootID
	for part := range strings.SplitSeq(relPath, "/") {
		if part == "" {
			continue
		}
		child, ok, err := c.findChildByName(ctx, cur, part, true)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("drive: parent folder does not exist")
		}
		cur = child.ID
	}
	return cur, nil
}

// resolveParentAndLeaf walks all but the last segment; optionally creates intermediate folders.
func (c *Client) resolveParentAndLeaf(ctx context.Context, rootID, relPath string, createParents bool) (parentID, leaf string, err error) {
	relPath = strings.Trim(strings.TrimSpace(relPath), "/")
	if relPath == "" {
		return "", "", fmt.Errorf("drive: object path is required")
	}
	parts := strings.Split(relPath, "/")
	leaf = parts[len(parts)-1]
	cur := rootID
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return "", "", fmt.Errorf("drive: invalid path segment")
		}
		child, ok, err := c.findChildByName(ctx, cur, part, true)
		if err != nil {
			return "", "", err
		}
		if !ok {
			if !createParents {
				return "", "", fmt.Errorf("drive: parent folder does not exist")
			}
			created, err := c.createFolder(ctx, cur, part)
			if err != nil {
				return "", "", err
			}
			cur = created.ID
			continue
		}
		cur = child.ID
	}
	return cur, leaf, nil
}

func (c *Client) createFileMultipart(ctx context.Context, parentID, name, localPath, contentType string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("drive: read local file: %w", err)
	}
	meta := fmt.Sprintf(`{"name":%q,"parents":[%q],"mimeType":%q}`, name, parentID, contentType)
	body, ctype := buildMultipart(meta, contentType, data)

	params := url.Values{}
	params.Set("uploadType", "multipart")
	params.Set("supportsAllDrives", "true")
	params.Set("fields", "id,name,mimeType,size,modifiedTime")
	u := c.uploadBase() + "/files?" + params.Encode()

	return c.doUpload(ctx, http.MethodPost, u, ctype, body)
}

func (c *Client) updateFileMedia(ctx context.Context, fileID, localPath, contentType string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("drive: read local file: %w", err)
	}
	meta := fmt.Sprintf(`{"mimeType":%q}`, contentType)
	body, ctype := buildMultipart(meta, contentType, data)

	params := url.Values{}
	params.Set("uploadType", "multipart")
	params.Set("supportsAllDrives", "true")
	params.Set("fields", "id,name,mimeType,size,modifiedTime")
	u := c.uploadBase() + "/files/" + url.PathEscape(fileID) + "?" + params.Encode()

	return c.doUpload(ctx, http.MethodPatch, u, ctype, body)
}

func buildMultipart(metaJSON, contentType string, data []byte) (body []byte, contentTypeHeader string) {
	var buf bytes.Buffer
	boundary := multipartBoundary
	// --boundary\r\n
	fmt.Fprintf(&buf, "--%s\r\n", boundary)
	fmt.Fprintf(&buf, "Content-Type: application/json; charset=UTF-8\r\n\r\n")
	buf.WriteString(metaJSON)
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "--%s\r\n", boundary)
	fmt.Fprintf(&buf, "Content-Type: %s\r\n\r\n", contentType)
	buf.Write(data)
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	return buf.Bytes(), "multipart/related; boundary=" + boundary
}

func (c *Client) doUpload(ctx context.Context, method, fullURL, contentType string, body []byte) error {
	if c == nil || c.Auth == nil {
		return fmt.Errorf("drive: client not configured")
	}
	token, err := c.Auth.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	resp, err := c.mediaHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapDriveError(method, resp.StatusCode, raw)
	}
	return nil
}

func (c *Client) downloadMedia(ctx context.Context, fileID, destPath string) error {
	if c == nil || c.Auth == nil {
		return fmt.Errorf("drive: client not configured")
	}
	token, err := c.Auth.Token(ctx)
	if err != nil {
		return err
	}
	params := url.Values{}
	params.Set("alt", "media")
	params.Set("supportsAllDrives", "true")
	u := c.apiBase() + "/files/" + url.PathEscape(fileID) + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.mediaHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return mapDriveError("GET", resp.StatusCode, raw)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

func contentTypeFor(name, localPath string) string {
	ext := path.Ext(name)
	if ext == "" {
		ext = filepath.Ext(localPath)
	}
	if ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return "application/octet-stream"
}
