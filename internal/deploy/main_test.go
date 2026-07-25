package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain poisons the deploy tools on PATH for this package.
//
// A deploy step execs whatever the manifest names. A test whose manifest says
// "kubectl apply" and forgets to point at a fake would reach the operator's real
// cluster: silent, irreversible, and exactly the kind of mistake that must be
// impossible rather than remembered. git and sh stay real — the runner and the
// manifest loader genuinely need them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gw-deploy-poison-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "poison bin:", err)
		os.Exit(1)
	}
	for _, name := range []string{"docker", "kubectl", "helm", "gcloud", "gsutil", "aws", "terraform"} {
		script := "#!/bin/sh\n" +
			"echo 'TEST BUG: execed the real " + name + ". Point the test manifest at a" +
			" fake script in t.TempDir().' >&2\n" +
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
