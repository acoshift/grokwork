package grokrun

import (
	"reflect"
	"testing"
)

func TestToolFlagsEmptyMeansToolsOff(t *testing.T) {
	empty := ""
	got := toolFlags(&empty)
	want := []string{"--deny", "MCPTool", "--tools", toolsOffAllowlist}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsNilOmits(t *testing.T) {
	if toolFlags(nil) != nil {
		t.Fatal(toolFlags(nil))
	}
}

func TestToolFlagsExplicitAllowlistDeniesMCP(t *testing.T) {
	s := "read_file,grep"
	got := toolFlags(&s)
	// Investigate-style non-empty allowlist must still deny MCP meta-tools.
	want := []string{"--deny", "MCPTool", "--tools", "read_file,grep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestToolFlagsAllNonNilDenyMCPTool(t *testing.T) {
	empty := ""
	allow := "read_file,grep"
	for _, tools := range []*string{&empty, &allow} {
		got := toolFlags(tools)
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
	if toolFlags(nil) != nil {
		t.Fatal("nil Tools must stay unrestricted (no deny flags)")
	}
}
