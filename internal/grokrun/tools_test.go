package grokrun

import (
	"reflect"
	"slices"
	"testing"
)

func TestToolFlagsEmptyMeansToolsOff(t *testing.T) {
	empty := ""
	got := toolFlags(&empty, false)
	want := []string{"--deny", "MCPTool", "--tools", toolsOffAllowlist}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsNilOmits(t *testing.T) {
	if toolFlags(nil, false) != nil {
		t.Fatal(toolFlags(nil, false))
	}
	if toolFlags(nil, true) != nil {
		t.Fatal(toolFlags(nil, true))
	}
}

func TestToolFlagsExplicitAllowlistDeniesMCP(t *testing.T) {
	s := "read_file,grep"
	got := toolFlags(&s, false)
	// Investigate-style non-empty allowlist must still deny MCP meta-tools.
	want := []string{"--deny", "MCPTool", "--tools", "read_file,grep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsAllowMCPOmitsDenyOnAllowlist(t *testing.T) {
	s := "read_file,grep"
	got := toolFlags(&s, true)
	want := []string{"--tools", "read_file,grep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsAllowMCPStillDeniesToolsOff(t *testing.T) {
	empty := ""
	got := toolFlags(&empty, true)
	want := []string{"--deny", "MCPTool", "--tools", toolsOffAllowlist}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsAllNonNilDenyMCPTool(t *testing.T) {
	empty := ""
	allow := "read_file,grep"
	for _, tools := range []*string{&empty, &allow} {
		got := toolFlags(tools, false)
		denied := false
		for i := 0; i+1 < len(got); i++ {
			if got[i] == "--deny" && got[i+1] == "MCPTool" {
				denied = true
				break
			}
		}
		if !denied {
			t.Fatalf("non-nil Tools=%q args %v must include --deny MCPTool", *tools, got)
		}
	}
	if toolFlags(nil, false) != nil {
		t.Fatal("nil Tools must stay unrestricted (no deny flags)")
	}
}

func grokArgs(opt Options) []string {
	return grokDriver{}.args(argInput{opt: opt, promptPath: "/tmp/p", format: "json"})
}

func TestGrokArgsAllowMCPOmitsDeny(t *testing.T) {
	tools := "read_file,grep"
	args := grokArgs(Options{Tools: &tools, AllowMCP: true, MaxTurns: 1})
	if slices.Contains(args, "--deny") {
		t.Fatalf("AllowMCP must omit --deny: %v", args)
	}
	if argValue(args, "--tools") != tools {
		t.Fatalf("tools=%q args=%v", argValue(args, "--tools"), args)
	}
}

func TestGrokArgsInvestigateStillDeniesMCP(t *testing.T) {
	tools := "read_file,grep"
	args := grokArgs(Options{Tools: &tools, MaxTurns: 1})
	if argValue(args, "--deny") != "MCPTool" {
		t.Fatalf("default investigate must deny MCPTool: %v", args)
	}
}
