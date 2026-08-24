package grokrun

import (
	"strings"
	"testing"
)

func TestSuggestConflictToolsNoShell(t *testing.T) {
	t.Parallel()
	for _, a := range []Agent{AgentGrok, AgentClaude, AgentCursor} {
		got := suggestConflictTools(a)
		for _, bad := range []string{"run_terminal_command", "Bash", "Shell"} {
			if strings.Contains(got, bad) {
				t.Errorf("agent %s tools %q must not include %s", a, got, bad)
			}
		}
	}
	if !strings.Contains(suggestConflictTools(AgentGrok), "write_file") {
		t.Fatal("grok must be allowed to write conflicted files")
	}
}
