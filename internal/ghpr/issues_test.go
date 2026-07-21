package ghpr

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestListIssuesWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "issue list") {
			t.Fatalf("unexpected: %v", args)
		}
		if !strings.Contains(joined, "--repo acme/app") {
			t.Fatalf("missing --repo: %v", args)
		}
		if !strings.Contains(joined, "closedByPullRequestsReferences") {
			t.Fatalf("list missing linked PR field: %v", args)
		}
		// Slim list fields (no body/labels) — matches production --json.
		return []byte(`[
			{"number":1,"url":"https://github.com/acme/app/issues/1","title":"Bug","state":"OPEN","author":{"login":"alice"},
			 "closedByPullRequestsReferences":[{"number":9,"url":"https://github.com/acme/app/pull/9","repository":{"name":"app","owner":{"login":"acme"}}}]},
			{"number":2,"url":"https://github.com/acme/app/issues/2","title":"Feat","state":"CLOSED","author":{"login":"bob"},
			 "closedByPullRequestsReferences":[]}
		]`), nil
	}
	list, err := ListIssuesWith(context.Background(), run, "/repo", IssueListOpts{Owner: "acme", Repo: "app", State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].Number != 1 || list[0].Title != "Bug" || list[0].Author != "alice" {
		t.Fatalf("first=%+v", list[0])
	}
	if list[0].State != "OPEN" {
		t.Fatalf("first state=%+v", list[0])
	}
	if len(list[0].LinkedPRs) != 1 || list[0].LinkedPRs[0].Number != 9 {
		t.Fatalf("first linked=%+v", list[0].LinkedPRs)
	}
	if list[0].LinkedPRs[0].Owner != "acme" || list[0].LinkedPRs[0].Repo != "app" {
		t.Fatalf("linked owner/repo=%+v", list[0].LinkedPRs[0])
	}
	if list[1].Number != 2 || list[1].State != "CLOSED" {
		t.Fatalf("second=%+v", list[1])
	}
	if len(list[1].LinkedPRs) != 0 {
		t.Fatalf("second linked=%+v", list[1].LinkedPRs)
	}
}

func TestViewIssueWithMock(t *testing.T) {
	var sawGraphQL bool
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "api graphql") {
			sawGraphQL = true
			if !strings.Contains(joined, "owner=acme") || !strings.Contains(joined, "repo=app") || !strings.Contains(joined, "number=42") {
				t.Fatalf("graphql vars: %v", args)
			}
			return []byte(`{
				"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[
					{"number":9,"title":"Fix payment timeout","url":"https://github.com/acme/app/pull/9","state":"OPEN","isDraft":false,
					 "repository":{"name":"app","owner":{"login":"acme"}}}
				]}}}}
			}`), nil
		}
		if !strings.Contains(joined, "issue view 42") {
			t.Fatalf("args=%v", args)
		}
		if !strings.Contains(joined, "closedByPullRequestsReferences") {
			t.Fatalf("view missing linked PR field: %v", args)
		}
		return []byte(`{
			"number":42,
			"url":"https://github.com/acme/app/issues/42",
			"title":"Payment timeout",
			"state":"OPEN",
			"author":{"login":"carol"},
			"labels":[{"name":"p0"}],
			"body":"Repro steps…",
			"comments":[
				{"author":{"login":"dave"},"body":"looking","url":"https://github.com/acme/app/issues/42#issuecomment-1"}
			],
			"closedByPullRequestsReferences":[
				{"number":9,"url":"https://github.com/acme/app/pull/9","repository":{"name":"app","owner":{"login":"acme"}}}
			]
		}`), nil
	}
	info, err := ViewIssueWith(context.Background(), run, "/repo", 42, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if info.Number != 42 || info.Title != "Payment timeout" || info.Author != "carol" {
		t.Fatalf("%+v", info)
	}
	if info.Body != "Repro steps…" {
		t.Fatalf("body=%q", info.Body)
	}
	if len(info.Comments) != 1 || info.Comments[0].Author != "dave" {
		t.Fatalf("comments=%+v", info.Comments)
	}
	if info.Owner != "acme" || info.Repo != "app" {
		t.Fatalf("owner/repo=%s/%s", info.Owner, info.Repo)
	}
	if !sawGraphQL {
		t.Fatal("expected GraphQL enrichment for linked PRs")
	}
	if len(info.LinkedPRs) != 1 {
		t.Fatalf("linked=%+v", info.LinkedPRs)
	}
	pr := info.LinkedPRs[0]
	if pr.Number != 9 || pr.Title != "Fix payment timeout" || pr.State != "OPEN" {
		t.Fatalf("linked pr=%+v", pr)
	}
	if pr.Owner != "acme" || pr.Repo != "app" || pr.URL == "" {
		t.Fatalf("linked owner/url=%+v", pr)
	}
}

func TestViewIssueLinkedPRsGraphQLFallback(t *testing.T) {
	// GraphQL fails → keep CLI-parsed linked PRs (number/url only).
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "api graphql") {
			return nil, fmt.Errorf("graphql down")
		}
		return []byte(`{
			"number":1,"url":"https://github.com/acme/app/issues/1","title":"t","state":"OPEN",
			"author":{"login":"a"},"labels":[],"body":"b","comments":[],
			"closedByPullRequestsReferences":[
				{"number":3,"url":"https://github.com/acme/other/pull/3",
				 "repository":{"name":"other","owner":{"login":"acme"}}}
			]
		}`), nil
	}
	info, err := ViewIssueWith(context.Background(), run, "/repo", 1, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.LinkedPRs) != 1 || info.LinkedPRs[0].Number != 3 {
		t.Fatalf("linked=%+v", info.LinkedPRs)
	}
	if info.LinkedPRs[0].Repo != "other" || info.LinkedPRs[0].Title != "" {
		t.Fatalf("expected CLI shape without title: %+v", info.LinkedPRs[0])
	}
}

func TestViewIssueBodyCap(t *testing.T) {
	big := strings.Repeat("x", DefaultIssueBodyCap+100)
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte(`{"number":1,"url":"https://github.com/o/r/issues/1","title":"t","state":"OPEN","author":{"login":"a"},"labels":[],"body":` +
			`"` + big + `","comments":[]}`), nil
	}
	// JSON with huge body - need proper JSON encoding
	run = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		body, _ := jsonMarshalBody(big)
		return []byte(`{"number":1,"url":"https://github.com/o/r/issues/1","title":"t","state":"OPEN","author":{"login":"a"},"labels":[],"body":` + body + `,"comments":[]}`), nil
	}
	info, err := ViewIssueWith(context.Background(), run, "/r", 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Truncated {
		t.Fatal("expected truncated")
	}
	if len(info.Body) > DefaultIssueBodyCap+8 {
		t.Fatalf("body still too long: %d", len(info.Body))
	}
	if info.Owner != "o" || info.Repo != "r" {
		t.Fatalf("fillOwnerRepo failed: %s/%s", info.Owner, info.Repo)
	}
}

func jsonMarshalBody(s string) (string, error) {
	// minimal JSON string escape
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			b = append(b, '\\', c)
		} else {
			b = append(b, c)
		}
	}
	b = append(b, '"')
	return string(b), nil
}
