package web

import (
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestSummaryPRsDisplayState(t *testing.T) {
	got := summaryPRs([]sessionstore.TrackedPR{
		{Number: 7, State: "open", Owner: "acme", Repo: "app", Checks: "✓ 1 · ✗ 2"},
		{Number: 8, State: "OPEN", IsDraft: true, Owner: "acme", Repo: "lib"},
		{Number: 9, State: "MERGED", Checks: "✗ 1", Owner: "acme", Repo: "app"},
		{Number: 0, URL: "https://example.com/not-a-number"},
	})
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (numberless PR dropped): %+v", len(got), got)
	}
	if got[0].State != "OPEN" || !got[0].Failing {
		t.Fatalf("open failing: %+v", got[0])
	}
	if got[1].State != "DRAFT" || got[1].Failing {
		t.Fatalf("draft: %+v", got[1])
	}
	if got[2].State != "MERGED" || got[2].Failing {
		t.Fatalf("merged must not stay CI failing: %+v", got[2])
	}
	if summaryPRs(nil) != nil {
		t.Fatal("empty input must stay nil")
	}
}

func TestApplySessionOverlayMigratesLegacyPR(t *testing.T) {
	var row history.Summary
	applySessionOverlay(&row, sessionstore.Listed{Entry: sessionstore.Entry{
		PRNumber: 4,
		PRState:  "CLOSED",
		PRURL:    "https://github.com/acme/app/pull/4",
	}})
	if len(row.PRs) != 1 || row.PRs[0].Number != 4 || row.PRs[0].State != "CLOSED" ||
		row.PRs[0].Owner != "acme" || row.PRs[0].Repo != "app" {
		t.Fatalf("legacy PR: %+v", row.PRs)
	}
	if row.PRNumber != 4 || row.PRState != "CLOSED" {
		t.Fatalf("primary slot: num=%d state=%q", row.PRNumber, row.PRState)
	}
}

func TestSessionRowsShowEveryPRState(t *testing.T) {
	srv, _, _ := authOnServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := srv.sessions.Set("th-prs", sessionstore.Entry{
		Project: "proj", OwnerID: "member-1", Goal: "two pull requests",
		Label: sessionstore.LabelInProgress, UpdatedAt: now,
		PRs: []sessionstore.TrackedPR{
			{Number: 7, State: "OPEN", Owner: "acme", Repo: "app", URL: "https://github.com/acme/app/pull/7", Checks: "✓ 1 · ✗ 2"},
			{Number: 8, State: "OPEN", IsDraft: true, Owner: "acme", Repo: "lib", URL: "https://github.com/acme/lib/pull/8"},
			{Number: 9, State: "MERGED", Owner: "acme", Repo: "app", URL: "https://github.com/acme/app/pull/9", Checks: "✗ 1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-legacy", sessionstore.Entry{
		Project: "proj", OwnerID: "member-1", Goal: "legacy single pr",
		UpdatedAt: now,
		PRNumber:  4, PRState: "CLOSED", PRURL: "https://github.com/acme/app/pull/4",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Set("th-none", sessionstore.Entry{
		Project: "proj", OwnerID: "member-1", Goal: "no pull request",
		Label: sessionstore.LabelOpen, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}

	wantLinks := []string{
		`class="badge status-error" title="acme/app#7" href="/prs/acme/app/7?project=proj">#7 · CI failing</a>`,
		`class="badge" title="acme/lib#8" href="/prs/acme/lib/8?project=proj">#8 · DRAFT</a>`,
		`class="badge status-merged" title="acme/app#9" href="/prs/acme/app/9?project=proj">#9 · MERGED</a>`,
		`class="badge status-cancelled" title="acme/app#4" href="/prs/acme/app/4?project=proj">#4 · CLOSED</a>`,
	}
	wantChips := []string{
		`<span class="badge status-error">#7 · CI failing</span>`,
		`<span class="badge">#8 · DRAFT</span>`,
		`<span class="badge status-merged">#9 · MERGED</span>`,
	}
	for _, path := range []string{
		"/sessions",
		"/projects/proj/sessions",
		"/today",
		"/projects/proj/today",
		"/partials/today/list",
		"/projects/proj",
	} {
		body := getPageBody(t, srv, sid, path)
		if strings.Contains(body, "#9 · CI failing") || strings.Contains(body, "#8 · OPEN") {
			t.Fatalf("%s showed the wrong PR state", path)
		}
		for _, want := range wantLinks {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
		// Phone rows are an <a>, so they use chips. Today cards use the links above.
		if path == "/sessions" || path == "/projects/proj/sessions" || path == "/projects/proj" {
			for _, want := range wantChips {
				if !strings.Contains(body, want) {
					t.Fatalf("%s missing chip %q", path, want)
				}
			}
		}
		if path == "/sessions" || path == "/projects/proj/sessions" {
			table := body[strings.Index(body, `id="sessions-table"`):]
			i := strings.Index(table, ">th-none</a>")
			if i < 0 {
				t.Fatalf("%s missing th-none row", path)
			}
			row := table[i:]
			end := strings.Index(row, "</tr>")
			if end < 0 || !strings.Contains(row[:end], `<td class="session-prs"><span class="muted">—</span></td>`) {
				t.Fatalf("%s session without a PR must keep the empty cell", path)
			}
		}
	}
}
