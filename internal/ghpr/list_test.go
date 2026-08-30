package ghpr

import (
	"context"
	"strings"
	"testing"
)

func TestListPRsWithMock(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		_ = ctx
		_ = dir
		_ = name
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "pr list") {
			t.Fatalf("unexpected: %v", args)
		}
		if !strings.Contains(joined, "--repo acme/app") {
			t.Fatalf("missing --repo: %v", args)
		}
		if !strings.Contains(joined, "--state open") {
			t.Fatalf("missing --state open: %v", args)
		}
		for _, field := range []string{"number", "url", "title", "state", "isDraft", "author", "reviewDecision", "headRefOid", "headRefName", "updatedAt"} {
			if !strings.Contains(joined, field) {
				t.Fatalf("list missing json field %s: %v", field, args)
			}
		}
		return []byte(`[
			{"number":12,"url":"https://github.com/acme/app/pull/12","title":"Human PR","state":"OPEN","isDraft":false,
			 "author":{"login":"zoe"},"reviewDecision":"REVIEW_REQUIRED","headRefOid":"abc","headRefName":"feat-x",
			 "updatedAt":"2026-08-30T10:00:00Z"},
			{"number":13,"url":"https://github.com/acme/app/pull/13","title":"Draft","state":"OPEN","isDraft":true,
			 "author":"kai","reviewDecision":"","headRefOid":"def","headRefName":"wip",
			 "updatedAt":"2026-08-29T09:00:00Z"}
		]`), nil
	}
	list, err := ListPRsWith(t.Context(), run, "/repo", PRListOpts{Owner: "acme", Repo: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].Number != 12 || list[0].Title != "Human PR" || list[0].Author != "zoe" {
		t.Fatalf("first=%+v", list[0])
	}
	if list[0].State != "OPEN" || list[0].Owner != "acme" || list[0].Repo != "app" {
		t.Fatalf("first identity=%+v", list[0])
	}
	if list[0].HeadRef != "feat-x" || list[0].HeadSHA != "abc" {
		t.Fatalf("first head=%+v", list[0])
	}
	if list[0].UpdatedAt.IsZero() || list[0].UpdatedAt.UTC().Format("2006-01-02 15:04") != "2026-08-30 10:00" {
		t.Fatalf("first updatedAt=%v", list[0].UpdatedAt)
	}
	if !list[1].IsDraft || list[1].Author != "kai" {
		t.Fatalf("second=%+v", list[1])
	}
}

func TestListPRsRequiresOwnerRepo(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("should not run gh")
		return nil, nil
	}
	if _, err := ListPRsWith(t.Context(), run, "/repo", PRListOpts{}); err == nil {
		t.Fatal("expected error")
	}
}
