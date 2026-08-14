package agentmcp

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShortUnixPathLeavesShort(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "a.sock")
	if got := ShortUnixPath(p); got != filepath.Clean(p) && len(p) <= maxUnixPath {
		if len(filepath.Clean(p)) <= maxUnixPath && got != filepath.Clean(p) {
			t.Fatalf("got %q want %q", got, p)
		}
	}
}

func TestShortUnixPathHashesLong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 80)
	p := filepath.Join("/var/folders/b9/rxg1mjw54hsdwtvp35ny1t880000gn/T", long, "data", "agent.sock")
	got := ShortUnixPath(p)
	if len(got) > maxUnixPath {
		t.Fatalf("still long: %d %q", len(got), got)
	}
	if !strings.Contains(got, "gw-mcp-") {
		t.Fatalf("got %q", got)
	}
}
