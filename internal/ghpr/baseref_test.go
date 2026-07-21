package ghpr

import (
	"context"
	"strings"
	"testing"
)

func TestPreferOriginRef(t *testing.T) {
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name != "git" || args[0] != "rev-parse" {
			t.Fatalf("%s %v", name, args)
		}
		ref := args[len(args)-1]
		if ref == "origin/prod" || ref == "main" {
			return []byte(ref + "\n"), nil
		}
		return nil, errRefMissing
	}
	if g := PreferOriginRef(context.Background(), run, "/r", "prod"); g != "origin/prod" {
		t.Fatalf("got %q", g)
	}
	if g := PreferOriginRef(context.Background(), run, "/r", "main"); g != "main" {
		t.Fatalf("local main: got %q", g)
	}
	if g := PreferOriginRef(context.Background(), run, "/r", "origin/prod"); g != "origin/prod" {
		t.Fatalf("already origin: got %q", g)
	}
}

type missingRefError struct{}

func (missingRefError) Error() string { return "missing" }

var errRefMissing = missingRefError{}

func TestDetectClosestBaseRef(t *testing.T) {
	// origin/main is far (100 commits), origin/prod is close (2).
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("%s %v", name, args)
		}
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "rev-parse":
			ref := args[len(args)-1]
			if ref == "origin/main" || ref == "origin/prod" {
				return []byte("ok\n"), nil
			}
			return nil, errRefMissing
		case args[0] == "merge-base" && args[1] == "origin/main":
			return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
		case args[0] == "merge-base" && args[1] == "origin/prod":
			return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
		case args[0] == "rev-list" && strings.Contains(joined, "aaaa"):
			return []byte("100\n"), nil
		case args[0] == "rev-list" && strings.Contains(joined, "bbbb"):
			return []byte("2\n"), nil
		default:
			t.Fatalf("unexpected git %v", args)
			return nil, nil
		}
	}
	got := DetectClosestBaseRef(context.Background(), run, "/wt")
	if got != "origin/prod" {
		t.Fatalf("got %q want origin/prod", got)
	}
}

func TestResolveDiffBaseRefPreferredWins(t *testing.T) {
	run := func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "git" && args[0] == "rev-parse":
			ref := args[len(args)-1]
			if strings.HasPrefix(ref, "origin/") {
				return []byte("ok\n"), nil
			}
			return nil, errRefMissing
		case name == "git" && args[0] == "merge-base" && args[1] == "origin/prod":
			return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
		case name == "git" && args[0] == "merge-base":
			// other candidates during fallback would work; preferred should win first
			return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
		case name == "git" && args[0] == "rev-list":
			return []byte("1\n"), nil
		default:
			t.Fatalf("unexpected %s %s", name, joined)
			return nil, nil
		}
	}
	got := ResolveDiffBaseRef(context.Background(), run, "/wt", "prod")
	if got != "origin/prod" {
		t.Fatalf("got %q", got)
	}
}

func TestPRBaseRefWith(t *testing.T) {
	run := func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		if name != "gh" || args[0] != "pr" || args[1] != "view" {
			t.Fatalf("%s %v", name, args)
		}
		if dir != "/wt" {
			t.Fatalf("dir=%q", dir)
		}
		return []byte(`{"baseRefName":"prod"}`), nil
	}
	b, err := PRBaseRefWith(context.Background(), run, "/wt", "https://github.com/o/r/pull/1")
	if err != nil || b != "prod" {
		t.Fatalf("base=%q err=%v", b, err)
	}
}
