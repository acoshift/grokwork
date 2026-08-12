package ghpr

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/acoshift/grokwork/internal/gitworktree"
)

// Pins Actions empty-preferred candidate order to gitworktree's list so the
// two cannot drift (same spirit as TestModelOptionsMatchInference).
func TestPrimaryBranchCandidatesMatchGitworktree(t *testing.T) {
	t.Parallel()
	want := gitworktree.PrimaryBranchCandidateNames
	if len(want) == 0 {
		t.Fatal("gitworktree candidates empty")
	}
	// resolveOriginPrimaryRef uses the same slice after origin/HEAD.
	if !slices.Contains(want, "prod") || !slices.Contains(want, "develop") {
		t.Fatalf("expected prod and develop in candidates: %v", want)
	}
	// Empty preferred path must try origin/<name> for each candidate name.
	var tried []string
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "rev-parse --verify ") {
			ref := args[len(args)-1]
			tried = append(tried, ref)
			return nil, fmt.Errorf("missing")
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	_, err := ResolveOriginPrimaryRef(t.Context(), run, "/repo", "")
	if err == nil {
		t.Fatal("expected error when nothing resolves")
	}
	if len(tried) == 0 || tried[0] != "refs/remotes/origin/HEAD" {
		t.Fatalf("first try want origin/HEAD, tried=%v", tried)
	}
	for _, name := range want {
		if !slices.Contains(tried, "origin/"+name) {
			t.Fatalf("empty preferred must try origin/%s; tried=%v", name, tried)
		}
	}
}

func TestResolveOriginPrimaryRefPreferred(t *testing.T) {
	t.Parallel()
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "rev-parse --verify origin/prod") {
			return []byte("sha-prod\n"), nil
		}
		return nil, fmt.Errorf("missing")
	}
	ref, err := ResolveOriginPrimaryRef(t.Context(), run, "/repo", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "sha-prod" {
		t.Fatalf("got %q", ref)
	}
	_, err = ResolveOriginPrimaryRef(t.Context(), run, "/repo", "missing")
	if err == nil {
		t.Fatal("expected error for missing preferred")
	}
}
