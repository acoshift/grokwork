package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoshift/grokwork/internal/config"
)

// TestMain poisons `grok` and `claude` on PATH for the whole package.
//
// A test that forgets to point GrokBin/ClaudeBin at a fake does not fail — an
// unset binary normalizes to the bare name and the driver execs the *operator's*
// real CLI: real token spend, `--permission-mode bypassPermissions`, and on the
// default 30-minute timeout an orphaned autonomous agent left running after the
// test binary exits. That is silent, so it must be impossible rather than
// remembered. The poison scripts fail instantly and name themselves in the run
// output. git/gh stay on the real PATH, which some tests genuinely need.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gw-poison-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "poison bin:", err)
		os.Exit(1)
	}
	for _, name := range []string{"grok", "claude"} {
		script := "#!/bin/sh\n" +
			"echo 'TEST BUG: execed the real " + name + " CLI. Point GrokBin/ClaudeBin at a fake" +
			" (writeFakeGrok / writeFakeClaude); note SetAgentSettings rewrites both.' >&2\n" +
			"exit 97\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "poison bin:", err)
			os.Exit(1)
		}
	}
	// Deploy steps exec whatever a manifest names. A test that forgets a fake
	// would otherwise reach the operator's real cluster or registry, which is
	// silent and irreversible — so make it impossible rather than remembered.
	// git and sh stay real: the deploy tests need both.
	for _, name := range []string{"docker", "kubectl", "helm", "gcloud", "gsutil", "aws"} {
		script := "#!/bin/sh\n" +
			"echo 'TEST BUG: execed the real " + name + ". A deploy test must point its" +
			" manifest at a fake script in t.TempDir().' >&2\n" +
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

// setAgentSettingsKeepBins applies model settings while keeping the fake binaries.
// SetAgentSettings persists the whole config form, so omitting GrokBin/ClaudeBin
// resets them to the bare names — see TestMain for why that matters.
func setAgentSettingsKeepBins(t *testing.T, cfg *config.Config, in config.AgentSettings) {
	t.Helper()
	snap := cfg.Snapshot()
	in.GrokBin, in.ClaudeBin = snap.GrokBin, snap.ClaudeBin
	if err := cfg.SetAgentSettings(in); err != nil {
		t.Fatal(err)
	}
}
