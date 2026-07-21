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

func TestToolFlagsExplicitAllowlist(t *testing.T) {
	s := "read_file,grep"
	got := toolFlags(&s)
	want := []string{"--tools", "read_file,grep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
