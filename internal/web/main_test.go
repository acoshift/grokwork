package web

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// TestMain poisons `grok` and `claude` on PATH for the whole package — see the
// same guard in internal/bot. A web test that starts a session and forgets to fake
// a binary would otherwise exec the operator's real CLI with permissions bypassed,
// and (on the default 30-minute timeout) orphan it when the test binary exits.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gw-poison-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "poison bin:", err)
		os.Exit(1)
	}
	for _, name := range []string{"grok", "claude"} {
		script := "#!/bin/sh\n" +
			"echo 'TEST BUG: execed the real " + name + " CLI. Point GrokBin/ClaudeBin at a fake" +
			" (writeWebFakeGrok / writeWebFakeClaude); note SetAgentSettings rewrites both.' >&2\n" +
			"exit 97\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "poison bin:", err)
			os.Exit(1)
		}
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// setAgentSettingsKeepBins applies model settings while keeping the fake binaries:
// SetAgentSettings persists the whole config form, so omitting GrokBin/ClaudeBin
// resets them to the bare names.
func setAgentSettingsKeepBins(t *testing.T, cfg *config.Config, in config.AgentSettings) {
	t.Helper()
	snap := cfg.Snapshot()
	in.GrokBin, in.ClaudeBin = snap.GrokBin, snap.ClaudeBin
	if err := cfg.SetAgentSettings(in); err != nil {
		t.Fatal(err)
	}
}
