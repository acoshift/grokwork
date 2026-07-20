package ghpr

import (
	"context"
	"strings"
	"testing"
)

func TestListUnresolvedReviewCommentsWith(t *testing.T) {
	raw := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "nodes": [
            {
              "isResolved": true,
              "isOutdated": false,
              "path": "a.go",
              "line": 1,
              "comments": { "nodes": [{ "body": "resolved", "url": "u1", "author": { "login": "x" } }] }
            },
            {
              "isResolved": false,
              "isOutdated": false,
              "path": "b.go",
              "line": 10,
              "comments": {
                "nodes": [
                  { "body": "first", "url": "u2a", "author": { "login": "a" } },
                  { "body": "please fix nil check", "url": "u2b", "author": { "login": "reviewer" } }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		if !strings.Contains(joined, "graphql") {
			t.Fatalf("unexpected: %s", joined)
		}
		if !strings.Contains(joined, "owner=acme") || !strings.Contains(joined, "repo=app") {
			t.Fatalf("missing vars: %s", joined)
		}
		return []byte(raw), nil
	}
	cs, err := ListUnresolvedReviewCommentsWith(context.Background(), run, "/repo", "acme", "app", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("want 1 unresolved, got %+v", cs)
	}
	if cs[0].Path != "b.go" || cs[0].Line != 10 || cs[0].Author != "reviewer" {
		t.Fatalf("%+v", cs[0])
	}
	if !strings.Contains(cs[0].Body, "nil check") {
		t.Fatalf("body=%q", cs[0].Body)
	}
}

func TestListUnresolvedReviewCommentsGraphQLError(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"errors":[{"message":"boom"}]}`), nil
	}
	_, err := ListUnresolvedReviewCommentsWith(context.Background(), run, "/r", "o", "r", 1)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestListUnresolvedReviewCommentsMissingPR(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":null}}`), nil
	}
	_, err := ListUnresolvedReviewCommentsWith(context.Background(), run, "/r", "o", "r", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListUnresolvedReviewCommentsInvalidArgs(t *testing.T) {
	_, err := ListUnresolvedReviewCommentsWith(context.Background(), nil, "/r", "", "r", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}
