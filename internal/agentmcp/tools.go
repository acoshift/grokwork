// Package agentmcp adapts agentapi to MCP tool names and a small JSON-RPC stdio loop.
package agentmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/acoshift/grokwork/internal/agentapi"
	"github.com/acoshift/grokwork/internal/agentauth"
)

// Tool names exposed to the coding CLI.
const (
	ToolSessionGet     = "session_get"
	ToolSessionDone    = "session_done"
	ToolSessionAbandon = "session_abandon"
	ToolPRsList        = "prs_list"
	ToolIssuesList     = "issues_list"
	ToolReviewRequest  = "review_request"
	ToolStoragePut     = "storage_put"
	ToolStorageGet     = "storage_get"
	ToolStorageList    = "storage_list"
	ToolStorageDelete  = "storage_delete"
	ToolClickUpGetTask = "clickup_get_task"
	ToolClickUpList    = "clickup_list_tasks"
	ToolLinearGetIssue = "linear_get_issue"
	ToolLinearList     = "linear_list_issues"
	ToolReviewersList  = "reviewers_list"
)

// ToolDef is an MCP tools/list entry.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolDefs returns the grokwork MCP tool catalog.
func ToolDefs() []ToolDef {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "integer"}
	return []ToolDef{
		{Name: ToolSessionGet, Description: "Get this bound session (goal, label, PRs).", InputSchema: obj(nil)},
		{Name: ToolSessionDone, Description: "Mark this session done (manual board label).", InputSchema: obj(nil)},
		{Name: ToolSessionAbandon, Description: "Soft-abandon this session (label + clear queue; keeps worktree).", InputSchema: obj(map[string]any{"reason": str})},
		{Name: ToolPRsList, Description: "List tracked PRs for session or project.", InputSchema: obj(map[string]any{"scope": str})},
		{Name: ToolIssuesList, Description: "List GitHub issues for the project repo.", InputSchema: obj(map[string]any{"state": str, "limit": num})},
		{Name: ToolReviewRequest, Description: "Request a team review (not GitHub formal).", InputSchema: obj(map[string]any{
			"owner": str, "repo": str, "number": num, "reviewerId": str, "note": str, "headSha": str,
		}, "owner", "repo", "number", "reviewerId")},
		{Name: ToolStoragePut, Description: "Put a project storage object. encoding is text (default) or base64.", InputSchema: obj(map[string]any{
			"key": str, "content": str, "contentType": str, "encoding": str,
		}, "key", "content")},
		{Name: ToolStorageGet, Description: "Get a project storage object.", InputSchema: obj(map[string]any{"key": str}, "key")},
		{Name: ToolStorageList, Description: "List project storage keys.", InputSchema: obj(map[string]any{"prefix": str, "limit": num})},
		{Name: ToolStorageDelete, Description: "Delete a project storage object.", InputSchema: obj(map[string]any{"key": str}, "key")},
		{Name: ToolClickUpGetTask, Description: "Get a ClickUp task by custom id, native id, or ClickUp URL. Uses the grokwork project key; do not call ClickUp HTTP.", InputSchema: obj(map[string]any{"ref": str}, "ref")},
		{Name: ToolClickUpList, Description: "List recent ClickUp tasks on this project's configured list.", InputSchema: obj(map[string]any{"limit": num})},
		{Name: ToolLinearGetIssue, Description: "Get one Linear issue by TEAM-N or a Linear issue URL. Uses this project's Linear key; do not call Linear HTTP.", InputSchema: obj(map[string]any{"ref": str}, "ref")},
		{Name: ToolLinearList, Description: "List recent Linear issues on this project's configured team.", InputSchema: obj(map[string]any{"limit": num})},
		{Name: ToolReviewersList, Description: "List team-review-eligible project members (canonical actor ids for review_request).", InputSchema: obj(nil)},
	}
}

func toolAllowed(name string, c agentauth.Caps) bool {
	switch name {
	case ToolSessionGet:
		return c.SessionRead
	case ToolSessionDone:
		return c.SessionDone
	case ToolSessionAbandon:
		return c.SessionAbandon
	case ToolPRsList:
		return c.PRsList
	case ToolIssuesList:
		return c.IssuesList
	case ToolReviewRequest, ToolReviewersList:
		return c.ReviewRequest
	case ToolStoragePut, ToolStorageDelete:
		return c.StorageWrite
	case ToolStorageGet, ToolStorageList:
		return c.StorageRead
	case ToolClickUpGetTask, ToolClickUpList:
		return c.ClickUpRead
	case ToolLinearGetIssue, ToolLinearList:
		return c.LinearRead
	default:
		return false
	}
}

// ToolDefsFor returns the catalog subset the minted caps allow.
func ToolDefsFor(c agentauth.Caps) []ToolDef {
	all := ToolDefs()
	out := make([]ToolDef, 0, len(all))
	for _, d := range all {
		if toolAllowed(d.Name, c) {
			out = append(out, d)
		}
	}
	return out
}

// CatalogForToken verifies the token and returns the cap-filtered catalog.
func CatalogForToken(svc *agentapi.Service, token string) ([]ToolDef, error) {
	if svc == nil || svc.Auth == nil {
		return nil, fmt.Errorf("service unavailable")
	}
	cred, err := svc.Auth.Verify(token)
	if err != nil {
		return nil, err
	}
	return ToolDefsFor(cred.Caps), nil
}

// Call dispatches a tool by name with JSON args object.
func Call(ctx context.Context, svc *agentapi.Service, token, name string, args map[string]any) (any, error) {
	if svc == nil {
		return nil, fmt.Errorf("service unavailable")
	}
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case ToolSessionGet:
		return svc.SessionGet(token)
	case ToolSessionDone:
		return map[string]string{"status": "ok"}, svc.SessionDone(token)
	case ToolSessionAbandon:
		return map[string]string{"status": "ok"}, svc.SessionAbandon(token, strArg(args, "reason"))
	case ToolPRsList:
		return svc.ListPRs(token, strArg(args, "scope"))
	case ToolIssuesList:
		limit := intArg(args, "limit")
		return svc.ListIssues(ctx, token, strArg(args, "state"), limit, nil)
	case ToolReviewRequest:
		n := intArg(args, "number")
		return svc.RequestTeamReview(token, strArg(args, "owner"), strArg(args, "repo"), n,
			strArg(args, "reviewerId"), strArg(args, "note"), strArg(args, "headSha"))
	case ToolStoragePut:
		return svc.StoragePut(token, strArg(args, "key"), strArg(args, "content"), strArg(args, "contentType"), strArg(args, "encoding"))
	case ToolStorageGet:
		b64, meta, err := svc.StorageGet(token, strArg(args, "key"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"contentBase64": b64, "meta": meta}, nil
	case ToolStorageList:
		return svc.StorageList(token, strArg(args, "prefix"), intArg(args, "limit"))
	case ToolStorageDelete:
		return map[string]string{"status": "ok"}, svc.StorageDelete(token, strArg(args, "key"))
	case ToolClickUpGetTask:
		return svc.GetClickUpTask(ctx, token, strArg(args, "ref"))
	case ToolClickUpList:
		return svc.ListClickUpTasks(ctx, token, intArg(args, "limit"))
	case ToolLinearGetIssue:
		return svc.GetLinearIssue(ctx, token, strArg(args, "ref"))
	case ToolLinearList:
		return svc.ListLinearIssues(ctx, token, intArg(args, "limit"))
	case ToolReviewersList:
		return svc.ListReviewers(token)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func strArg(args map[string]any, k string) string {
	v, ok := args[k]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func intArg(args map[string]any, k string) int {
	v, ok := args[k]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}
