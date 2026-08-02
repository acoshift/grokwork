package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

func TestPRAgentReviewCreatesWebNativeSession(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	e, ok := srv.sessions.Get(tid)
	if !ok || e.Project != "proj" {
		t.Fatalf("session missing: ok=%v %+v", ok, e)
	}
	// The goal is the only label the sessions list has for a web-native unit.
	if !strings.Contains(e.Goal, "acme/app#9") {
		t.Fatalf("goal=%q", e.Goal)
	}
	// Binds the PR for the detail Sessions list; SessionKind keeps it out of Address reuse.
	if !e.IsPRReview() {
		t.Fatalf("want SessionKindPRReview, got kind=%q", e.SessionKind)
	}
	e.NormalizePRs()
	if len(e.PRs) != 1 || e.PRs[0].Number != 9 {
		t.Fatalf("want PR bind, got %+v", e.PRs)
	}
	assertAuditAction(t, srv, audit.ActionPRReviewStart, true)
}

// After Review in new session, the PR detail page lists the review unit under Sessions.
func TestPRDetailShowsReviewSessions(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))

	// A work session on the same PR must also appear, without a review badge.
	work := sessionstore.Entry{Project: "proj", Goal: "CI acme/app#9", OwnerName: "bob"}
	work.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("work-pr-9", work); err != nil {
		t.Fatal(err)
	}

	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	if !strings.Contains(body, `id="pr-sessions"`) {
		t.Fatal("missing pr-sessions section")
	}
	if !strings.Contains(body, "Sessions (2)") {
		t.Fatalf("want Sessions (2); snippet: %s", bodySnippet(body, "pr-sessions", 400))
	}
	for _, want := range []string{
		"Review acme/app#9",
		"CI acme/app#9",
		`class="badge">review</span>`,
		`href="/sessions/` + tid + `?project=proj&amp;back=`,
		`back=` + url.QueryEscape("/prs/acme/app/9?project=proj"),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// Address dropdown must offer only the work unit, not the review.
	if i := strings.Index(body, `id="dispatch-thread"`); i >= 0 {
		chunk := body[i:]
		if j := strings.Index(chunk, "</select>"); j >= 0 {
			chunk = chunk[:j]
		}
		if strings.Contains(chunk, tid) {
			t.Fatalf("review unit must not be in Address session dropdown: %s", chunk)
		}
		if !strings.Contains(chunk, "work-pr-9") {
			t.Fatalf("work unit must be in Address session dropdown: %s", chunk)
		}
	} else {
		t.Fatal("expected Address session dropdown when a work unit binds the PR")
	}
}

// A review is a fresh read of the current head, so it never joins the session that
// owns the PR — otherwise it could not be dispatched at all while that session was
// mid-run, and it would inherit an older diff's reasoning.
func TestPRAgentReviewNeverReusesBoundSession(t *testing.T) {
	srv, b := addressEnabledServer(t)
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	bound := sessionstore.Entry{Project: "proj"}
	bound.UpsertPR(sessionstore.TrackedPR{Owner: "acme", Repo: "app", Number: 9, State: "OPEN"})
	if err := srv.sessions.Set("owns-pr-9", bound); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tid := webUnitFromLocation(t, w.Header().Get("Location"))
	if tid == "owns-pr-9" {
		t.Fatal("review must not reuse the session bound to the PR")
	}
	// Binds the PR for the Sessions list, but SessionKind keeps it out of Address reuse.
	e, _ := srv.sessions.Get(tid)
	e.NormalizePRs()
	if !e.IsPRReview() || len(e.PRs) != 1 {
		t.Fatalf("want PR-review bind, got kind=%q prs=%+v", e.SessionKind, e.PRs)
	}
	hits := b.FindByPR("proj", "acme", "app", 9, true)
	if len(hits) != 1 || hits[0].ThreadID != "owns-pr-9" {
		t.Fatalf("Address reuse must only see the work unit, got %+v", hits)
	}
}

func TestPRAgentReviewFeatureOff404(t *testing.T) {
	srv, _, _ := authOnServer(t) // startSessions false
	sid, csrf, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid, csrf, url.Values{"project": {"proj"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// The review card carries its own model select + confirm modal (the Address pair
// share one form, so a third one is easy to forget), and the caps gate must hold on
// the POST — hiding the field is not a permission check.
func TestPRDetailReviewButtonAndModelGate(t *testing.T) {
	srv, b := addressEnabledServer(t)
	cfg := srv.cfg
	t.Cleanup(func() { bot.WaitIdleForTest(b, 5*time.Second) })
	_ = cfg.AddProjectAllowedUser("proj", "member-1")
	_ = cfg.AddProjectAllowedUser("proj", "member-2")
	if err := cfg.SetProjectCapabilityByUser("proj", "member-1", "builder"); err != nil {
		t.Fatal(err)
	}
	setAgentSettingsKeepBins(t, cfg, config.AgentSettings{
		Agent: "grok", Model: "grok-4.5", ReviewModel: "claude-opus-5",
	})
	sid, _, err := srv.LoginAs("member-1", "M", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	body := getPageBody(t, srv, sid, "/prs/acme/app/9?project=proj")
	for _, want := range []string{
		`action="/prs/acme/app/9/agent-review"`,
		`id="btn-review-pr"`,
		`data-confirm-title="Review PR"`,
		`>Review in new session</button>`,
		// The copy must state the two things that distinguish this from the commit
		// review card and the Address pair.
		"single PR comment",
		"no GitHub issues",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR detail missing %q", want)
		}
	}
	// Address CI, Address review, and Review each own a confirm-select.
	if got := strings.Count(body, `data-confirm-select="model"`); got < 3 {
		t.Fatalf("want a confirm-select on all three dispatch buttons, got %d", got)
	}
	if got := strings.Count(body, `<select name="model" hidden>`); got < 2 {
		t.Fatalf("the review form needs its own hidden select to clone from, got %d", got)
	}

	// Non-builder: no model field, and a forged model is refused on the POST.
	if err := cfg.SetProjectCapabilityByUser("proj", "member-2", "investigator"); err != nil {
		t.Fatal(err)
	}
	sid2, csrf2, err := srv.LoginAs("member-2", "M2", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if body2 := getPageBody(t, srv, sid2, "/prs/acme/app/9?project=proj"); strings.Contains(body2, `name="model"`) {
		t.Fatal("investigator must not see the model field on the review card")
	}
	w := postFix(t, srv, "/prs/acme/app/9/agent-review", sid2, csrf2, url.Values{
		"project": {"proj"}, "model": {"claude-opus-5"},
	})
	assertRedirectErr(t, w, "/prs/acme/app/9", "not allowed to pick a model")

	// An empty model stays available to a non-builder: only *choosing* is gated.
	w = postFix(t, srv, "/prs/acme/app/9/agent-review", sid2, csrf2, url.Values{"project": {"proj"}})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("default model must stay available to non-builders, got %q", loc)
	}
}
