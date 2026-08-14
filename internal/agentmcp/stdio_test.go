package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/agentauth"
)

func TestRunStdioMissingEnvCallIsError(t *testing.T) {
	t.Parallel()
	call := func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errNotAttached
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"clickup_get_task","arguments":{"ref":"DEV-1"}}}
`)
	var out bytes.Buffer
	if err := RunStdio(t.Context(), call, "", in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%d %q", len(lines), out.String())
	}
	var init rpcMsg
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil || init.Error != nil {
		t.Fatalf("init: %s", lines[0])
	}
	var list rpcMsg
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil || list.Error != nil {
		t.Fatalf("list: %s", lines[1])
	}
	var callResp struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil || !callResp.Result.IsError {
		t.Fatalf("call: %s", lines[2])
	}
}

func TestRunStdioListUsesFilteredCatalog(t *testing.T) {
	t.Parallel()
	call := func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errNotAttached
	}
	list := func(context.Context, string) []ToolDef {
		return ToolDefsFor(agentauth.DefaultInvestigateCaps())
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := RunStdioList(t.Context(), call, list, "tok", in, &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []ToolDef `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Result.Tools {
		if d.Name == ToolSessionDone {
			t.Fatal("filtered list advertised session_done")
		}
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatal("expected read tools")
	}
}

type attachedErr string

func (e attachedErr) Error() string { return string(e) }

const errNotAttached attachedErr = "not a grokwork-attached run"
