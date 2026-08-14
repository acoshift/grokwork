package agentapi

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/acoshift/grokwork/internal/clickup"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

const clickupDescMaxRunes = 8000

// ClickUpTask is the MCP-facing snapshot (no secrets).
type ClickUpTask struct {
	ID          string `json:"id,omitempty"`
	CustomID    string `json:"customId,omitempty"`
	Name        string `json:"name,omitempty"`
	Status      string `json:"status,omitempty"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	ListID      string `json:"listId,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

// ClickUpTaskRow is a list row without description.
type ClickUpTaskRow struct {
	ID       string `json:"id,omitempty"`
	CustomID string `json:"customId,omitempty"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"`
	URL      string `json:"url,omitempty"`
}

func taskFromClickUp(t clickup.Task) ClickUpTask {
	return ClickUpTask{
		ID:          t.ID,
		CustomID:    t.CustomID,
		Name:        t.Name,
		Status:      t.Status,
		URL:         t.URL,
		Description: capRunes(t.Description, clickupDescMaxRunes),
		ListID:      t.ListID,
		WorkspaceID: t.WorkspaceID,
	}
}

func (s *Service) clickupClient(project string) (*clickup.Client, error) {
	if s.ClickUpEnabled != nil && !s.ClickUpEnabled(project) {
		return nil, fmt.Errorf("clickup is not enabled for this project")
	}
	key := ""
	if s.ClickUpAPIKey != nil {
		key = s.ClickUpAPIKey(project)
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("clickup: no API key")
	}
	if s.ClickUpNew != nil {
		return s.ClickUpNew(key), nil
	}
	return clickup.New(key), nil
}

// GetClickUpTask resolves a ClickUp ref for the token's project.
func (s *Service) GetClickUpTask(ctx context.Context, token, ref string) (ClickUpTask, error) {
	cred, err := s.verify(token)
	if err != nil {
		return ClickUpTask{}, err
	}
	if !cred.Caps.ClickUpRead {
		return ClickUpTask{}, fmt.Errorf("forbidden: clickup read")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ClickUpTask{}, fmt.Errorf("ref required")
	}
	native, custom, urlWS, err := parseClickUpRef(ref)
	if err != nil {
		return ClickUpTask{}, err
	}
	client, err := s.clickupClient(cred.Project)
	if err != nil {
		return ClickUpTask{}, err
	}
	var got clickup.Task
	switch {
	case custom != "":
		ws := strings.TrimSpace(urlWS)
		if ws == "" && s.ClickUpWorkspaceID != nil {
			ws = s.ClickUpWorkspaceID(cred.Project)
		}
		if ws == "" {
			return ClickUpTask{}, fmt.Errorf("clickup: workspaceId required for custom id")
		}
		got, err = client.GetTask(ctx, custom, clickup.GetOpts{CustomTaskIDs: true, WorkspaceID: ws, Markdown: true})
	default:
		got, err = client.GetTask(ctx, native, clickup.GetOpts{Markdown: true})
	}
	if err != nil {
		return ClickUpTask{}, err
	}
	return taskFromClickUp(got), nil
}

// ListClickUpTasks lists the project's configured ClickUp list.
func (s *Service) ListClickUpTasks(ctx context.Context, token string, limit int) ([]ClickUpTaskRow, error) {
	cred, err := s.verify(token)
	if err != nil {
		return nil, err
	}
	if !cred.Caps.ClickUpRead {
		return nil, fmt.Errorf("forbidden: clickup read")
	}
	listID := ""
	if s.ClickUpListID != nil {
		listID = s.ClickUpListID(cred.Project)
	}
	if strings.TrimSpace(listID) == "" {
		return nil, fmt.Errorf("clickup: no list id")
	}
	client, err := s.clickupClient(cred.Project)
	if err != nil {
		return nil, err
	}
	tasks, err := client.ListTasks(ctx, listID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ClickUpTaskRow, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, ClickUpTaskRow{
			ID: t.ID, CustomID: t.CustomID, Name: t.Name, Status: t.Status, URL: t.URL,
		})
	}
	return out, nil
}

// parseClickUpRef classifies an explicit tool ref. PrefixParseEnabled is a
// free-text bind heuristic and is not used here.
func parseClickUpRef(ref string) (native, custom, urlWorkspace string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", fmt.Errorf("empty ClickUp ref")
	}
	if parsed := sessionstore.ParseClickUpIssueRefs(ref, ""); len(parsed) > 0 {
		iss := parsed[0]
		if cid := sessionstore.NormalizeClickUpCustomID(iss.CustomID); cid != "" {
			return "", cid, strings.TrimSpace(iss.WorkspaceID), nil
		}
		if nid := strings.TrimSpace(iss.ClickUpID); nid != "" {
			return nid, "", "", nil
		}
	}
	if clickup.LooksLikeCustomID(ref) {
		return "", sessionstore.NormalizeClickUpCustomID(ref), "", nil
	}
	return ref, "", "", nil
}

func capRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for idx := range s {
		if i == n {
			return s[:idx]
		}
		i++
	}
	return s
}
