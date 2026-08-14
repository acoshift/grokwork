package agentmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// JSON-RPC 2.0 message (subset for MCP stdio).
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// CallFunc is tools/call dispatch (token is fixed for the process).
type CallFunc func(ctx context.Context, token, name string, args map[string]any) (any, error)

// RunStdio drives MCP over newline-delimited JSON-RPC.
func RunStdio(ctx context.Context, call CallFunc, token string, r io.Reader, w io.Writer) error {
	if call == nil {
		return fmt.Errorf("nil call")
	}
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method == "" {
			continue
		}
		// Notifications (no id) that need no reply.
		if msg.ID == nil && (msg.Method == "notifications/initialized" || msg.Method == "initialized") {
			continue
		}
		resp := rpcMsg{JSONRPC: "2.0", ID: msg.ID}
		switch msg.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "grokwork", "version": "1"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": ToolDefs()}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			out, err := call(ctx, token, p.Name, p.Arguments)
			if err != nil {
				resp.Result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				}
			} else {
				raw, _ := json.Marshal(out)
				resp.Result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(raw)}},
					"isError": false,
				}
			}
		case "ping":
			resp.Result = map[string]any{}
		default:
			if msg.ID == nil {
				continue
			}
			resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", msg.Method)}
		}
		if msg.ID == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// RunStdioDefault serves MCP on stdin/stdout.
func RunStdioDefault(ctx context.Context, call CallFunc, token string) error {
	return RunStdio(ctx, call, token, os.Stdin, os.Stdout)
}
