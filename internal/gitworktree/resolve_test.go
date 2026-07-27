package gitworktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkRepo makes dir look like a git root to IsRepo (filesystem probe only).
func mkRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolveLocalRepoRefusesToLeaveTheProject is a containment rule, not a
// validation nicety. repo reaches here from a `?repo=` query string on the
// commits browser, the commit detail page and search; the only upstream check
// is a catalog lookup that config.ResolveRepoPicker skips entirely when the
// catalog is empty (an unconfigured multi-repo project, or a discovery
// failure). A joined `../other-project` would then resolve to a checkout the
// viewer's project ACL was never consulted about, because the ACL only sees
// the project that was named in the path.
func TestResolveLocalRepoRefusesToLeaveTheProject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "public") // multi-repo folder, not a repo itself
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := mkRepo(t, filepath.Join(root, "secret")) // another project entirely
	child := mkRepo(t, filepath.Join(project, "api"))   // a legitimate child checkout

	for _, repo := range []string{
		"../secret",        // the plain escape
		"..",               // the parent folder holding every project
		".",                // the project folder restated
		"../secret/",       // a trailing slash does not make it one element
		"api/../../secret", // cleans to the sibling
		"/" + sibling,      // absolute-looking
	} {
		got, err := ResolveLocalRepo(ctx, project, "x", repo)
		if err == nil {
			t.Fatalf("repo %q resolved to %q, want rejection", repo, got)
		}
		if !strings.Contains(err.Error(), "single path element") {
			t.Fatalf("repo %q rejected for the wrong reason: %v", repo, err)
		}
		if got != "" {
			t.Fatalf("repo %q returned a path %q alongside its error", repo, got)
		}
	}

	// A single-repo project reads repo not at all, but must still refuse the
	// shape: returning the project root for `../secret` would tell the caller
	// its traversal was accepted, and the answer must not depend on layout.
	single := mkRepo(t, filepath.Join(root, "single"))
	if _, err := ResolveLocalRepo(ctx, single, "x", "../secret"); err == nil {
		t.Fatal("a single-repo project accepted a traversing repo name")
	}

	// The legitimate case is untouched: one clean element naming a child.
	got, err := ResolveLocalRepo(ctx, project, "x", "api")
	if err != nil {
		t.Fatalf("named child rejected: %v", err)
	}
	if got != child {
		t.Fatalf("named child resolved to %q, want %q", got, child)
	}
	if got, err := ResolveLocalRepo(ctx, single, "x", "whatever"); err != nil || got != single {
		t.Fatalf("single-repo project resolved to (%q, %v), want (%q, nil)", got, err, single)
	}
}
